package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/services/internal/awsv4"
	"github.com/bindle-ai/bindle/services/internal/model"
	"github.com/bindle-ai/bindle/services/internal/payload"
	"github.com/bindle-ai/bindle/services/internal/pipeline"
)

// ---- fake Kinesis --------------------------------------------------------

type fakeShard struct {
	id, parent string
	closed     bool
	records    []Record
}

type fakeKinesis struct {
	mu       sync.Mutex
	shards   []*fakeShard
	throttle bool
	seq      int
}

func (f *fakeKinesis) shard(id string) *fakeShard {
	for _, s := range f.shards {
		if s.id == id {
			return s
		}
	}
	return nil
}

func (f *fakeKinesis) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(403)
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	fail := func(status int, typ string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": typ})
	}
	switch strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "Kinesis_20131202.") {
	case "PutRecord":
		if f.throttle {
			fail(400, "ProvisionedThroughputExceededException")
			return
		}
		var open []*fakeShard
		for _, s := range f.shards {
			if !s.closed {
				open = append(open, s)
			}
		}
		h := fnv.New32a()
		h.Write([]byte(in["PartitionKey"].(string)))
		s := open[int(h.Sum32())%len(open)]
		data, _ := json.Marshal(in["Data"]) // base64 string
		var raw []byte
		_ = json.Unmarshal(data, &raw)
		f.seq++
		s.records = append(s.records, Record{SequenceNumber: fmt.Sprintf("%020d", f.seq), PartitionKey: in["PartitionKey"].(string), Data: raw})
		_ = json.NewEncoder(w).Encode(map[string]any{"ShardId": s.id})
	case "ListShards":
		var out []Shard
		for _, s := range f.shards {
			out = append(out, Shard{ShardID: s.id, ParentShardID: s.parent})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Shards": out})
	case "GetShardIterator":
		s := f.shard(in["ShardId"].(string))
		pos := 0
		if in["ShardIteratorType"] == "AFTER_SEQUENCE_NUMBER" {
			for i, r := range s.records {
				if r.SequenceNumber == in["StartingSequenceNumber"] {
					pos = i + 1
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ShardIterator": s.id + "|" + strconv.Itoa(pos)})
	case "GetRecords":
		parts := strings.Split(in["ShardIterator"].(string), "|")
		s := f.shard(parts[0])
		pos, _ := strconv.Atoi(parts[1])
		end := min(pos+int(in["Limit"].(float64)), len(s.records))
		next := s.id + "|" + strconv.Itoa(end)
		if s.closed && end >= len(s.records) {
			next = ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Records": s.records[pos:end], "NextShardIterator": next})
	default:
		fail(400, "UnknownOperationException")
	}
}

type creds struct{}

func (creds) Retrieve(context.Context) (awsv4.Credentials, error) {
	return awsv4.Credentials{AccessKeyID: "AKID", SecretAccessKey: "s"}, nil
}

func newFake(t *testing.T, shards ...*fakeShard) (*fakeKinesis, *Kinesis) {
	f := &fakeKinesis{shards: shards}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Kinesis{Region: "us-west-2", Creds: creds{}, Endpoint: srv.URL}
}

// ---- in-memory leases and writer ----------------------------------------

type memLeases struct {
	mu sync.Mutex
	m  map[string]*memLease
}

type memLease struct {
	owner      string
	expires    time.Time
	checkpoint string
	finished   bool
}

func (l *memLeases) get(shard string) *memLease {
	if l.m == nil {
		l.m = map[string]*memLease{}
	}
	if l.m[shard] == nil {
		l.m[shard] = &memLease{}
	}
	return l.m[shard]
}

func (l *memLeases) Acquire(_ context.Context, _, shard, owner string, ttl time.Duration) (Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.get(shard)
	if e.owner != "" && e.owner != owner && time.Now().Before(e.expires) {
		return Lease{}, false, nil
	}
	e.owner, e.expires = owner, time.Now().Add(ttl)
	return Lease{Checkpoint: e.checkpoint, Finished: e.finished}, true, nil
}

func (l *memLeases) Renew(_ context.Context, _, shard, owner string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.get(shard)
	if e.owner != owner {
		return false, nil
	}
	e.expires = time.Now().Add(ttl)
	return true, nil
}

func (l *memLeases) Checkpoint(_ context.Context, _, shard, owner, seq string, finished bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.get(shard)
	if e.owner != owner {
		return ErrLeaseLost
	}
	e.checkpoint, e.finished = seq, finished
	return nil
}

func (l *memLeases) Finished(_ context.Context, _, shard string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.get(shard).finished, nil
}

type memWriter struct {
	mu      sync.Mutex
	steps   []model.Step
	byOwner map[string]int
	failN   int
	calls   int
}

func (w *memWriter) Write(_ context.Context, steps []model.Step) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.failN > 0 {
		w.failN--
		return errors.New("opensearch down")
	}
	w.steps = append(w.steps, steps...)
	return nil
}

func (w *memWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.steps)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func steps(tenant string, n int) []model.Step {
	out := make([]model.Step, n)
	for i := range out {
		out[i] = model.Step{TenantID: tenant, TraceID: "tr", StepID: fmt.Sprintf("%s-%d", tenant, i), Input: "hello"}
	}
	return out
}

// ---- tests ---------------------------------------------------------------

func TestEnvelopeInlineAndSpooled(t *testing.T) {
	ctx := context.Background()
	in := steps("t", 3)
	data, err := Encode(ctx, in, nil, time.Now())
	if err != nil || data[0] != 0x1f {
		t.Fatalf("inline: %v", err)
	}
	out, err := Decode(ctx, data, nil)
	if err != nil || len(out) != 3 || out[2].StepID != "t-2" {
		t.Fatalf("roundtrip: %v %v", out, err)
	}
	// Incompressible content larger than a record goes to the spool.
	big := steps("t", 1)
	big[0].Input = randomText(2 << 20) // ~1.2 MB gzipped
	spool := &payload.FileStore{Dir: t.TempDir()}
	if _, err := Encode(ctx, big, nil, time.Now()); err == nil {
		t.Fatal("oversized batch without spool must fail")
	}
	ptr, err := Encode(ctx, big, spool, time.Now())
	if err != nil || len(ptr) > 200 || !strings.Contains(string(ptr), `"spool":"spool/`) {
		t.Fatalf("pointer: %s %v", ptr, err)
	}
	out, err = Decode(ctx, ptr, spool)
	if err != nil || out[0].Input != big[0].Input {
		t.Fatalf("spooled roundtrip: %v", err)
	}
	for _, bad := range []string{`{"v":1,"spool":"../../etc/passwd"}`, `{"v":1,"spool":"tenants/x/secret"}`, `{"v":2}`, `garbage`} {
		if _, err := Decode(ctx, []byte(bad), spool); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func randomText(n int) string {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = 'a' + byte(x%26)
	}
	return string(b)
}

func TestProducerBackpressure(t *testing.T) {
	f, k := newFake(t, &fakeShard{id: "s0"})
	p := &Producer{Kinesis: k, Stream: "steps"}
	if err := p.Submit(steps("t", 2)); err != nil {
		t.Fatal(err)
	}
	if len(f.shards[0].records) != 1 {
		t.Fatal("one record per request")
	}
	f.throttle = true
	if err := p.Submit(steps("t", 1)); !errors.Is(err, pipeline.ErrBackpressure) {
		t.Fatalf("throttle: %v", err)
	}
	if err := p.Submit(nil); err != nil {
		t.Fatal("empty submit")
	}
}

func TestConsumerEndToEndWithRestartAndRetry(t *testing.T) {
	_, k := newFake(t, &fakeShard{id: "s0"}, &fakeShard{id: "s1"})
	p := &Producer{Kinesis: k, Stream: "steps"}
	for i := 0; i < 20; i++ {
		if err := p.Submit(steps(fmt.Sprintf("tenant-%d", i), 5)); err != nil {
			t.Fatal(err)
		}
	}
	leases := &memLeases{}
	w := &memWriter{failN: 2} // OpenSearch blips: must retry, not skip
	run := func(owner string) (context.CancelFunc, chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		c := &Consumer{Kinesis: k, Stream: "steps", Leases: leases, Writer: w, Owner: owner, Log: discard(),
			Poll: 10 * time.Millisecond, Discover: 20 * time.Millisecond, LeaseTTL: time.Second}
		go func() { c.Run(ctx); close(done) }()
		return cancel, done
	}
	cancel, done := run("p1")
	waitFor(t, func() bool { return w.count() == 100 })
	cancel()
	<-done
	if w.calls < 3 {
		t.Fatalf("write failures were not retried: %d calls", w.calls)
	}
	// Restart from checkpoints: nothing is written twice.
	cancel, done = run("p1")
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	if w.count() != 100 {
		t.Fatalf("restart re-wrote %d steps", w.count()-100)
	}
}

func TestConsumerReadsParentBeforeChild(t *testing.T) {
	f, k := newFake(t, &fakeShard{id: "parent", closed: true}, &fakeShard{id: "child", parent: "parent"})
	mk := func(id string) Record {
		d, _ := Encode(context.Background(), []model.Step{{TenantID: "t", StepID: id}}, nil, time.Now())
		return Record{SequenceNumber: id, Data: d}
	}
	f.shards[0].records = []Record{mk("p1"), mk("p2")}
	f.shards[1].records = []Record{mk("c1")}
	w := &memWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Consumer{Kinesis: k, Stream: "s", Leases: &memLeases{}, Writer: w, Owner: "p", Log: discard(),
		Poll: 10 * time.Millisecond, Discover: 20 * time.Millisecond}
	go c.Run(ctx)
	waitFor(t, func() bool { return w.count() == 3 })
	if w.steps[0].StepID != "p1" || w.steps[1].StepID != "p2" || w.steps[2].StepID != "c1" {
		t.Fatalf("order: %v %v %v", w.steps[0].StepID, w.steps[1].StepID, w.steps[2].StepID)
	}
}

func TestTwoConsumersShareShards(t *testing.T) {
	_, k := newFake(t, &fakeShard{id: "s0"}, &fakeShard{id: "s1"}, &fakeShard{id: "s2"})
	p := &Producer{Kinesis: k, Stream: "steps"}
	for i := 0; i < 30; i++ {
		_ = p.Submit(steps(fmt.Sprintf("t%d", i), 1))
	}
	leases := &memLeases{}
	w := &memWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, owner := range []string{"a", "b"} {
		c := &Consumer{Kinesis: k, Stream: "steps", Leases: leases, Writer: w, Owner: owner, Log: discard(),
			Poll: 10 * time.Millisecond, Discover: 15 * time.Millisecond, LeaseTTL: time.Second}
		go c.Run(ctx)
	}
	waitFor(t, func() bool { return w.count() >= 30 })
	time.Sleep(100 * time.Millisecond)
	if w.count() != 30 {
		t.Fatalf("shards consumed twice: %d steps", w.count())
	}
}
