// Package stream moves normalized steps from ingest to the processor
// through Amazon Kinesis Data Streams: a producer for ingest and a
// lease-based consumer for the processor. Standard library only.
package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
)

// Kinesis calls the Kinesis JSON API, signed with SigV4.
type Kinesis struct {
	Region   string
	Creds    awsv4.CredentialsProvider
	HTTP     *http.Client
	Endpoint string // tests only
}

// APIError is a Kinesis error response.
type APIError struct {
	Status int
	Type   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kinesis: HTTP %d %s: %s", e.Status, e.Type, e.Msg)
}

// Throttled reports whether err means "slow down".
func Throttled(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (strings.Contains(e.Type, "ProvisionedThroughputExceeded") || strings.Contains(e.Type, "LimitExceeded") || e.Status == 503)
}

func (k *Kinesis) call(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	u := k.Endpoint
	if u == "" {
		u = "https://kinesis." + k.Region + ".amazonaws.com/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Kinesis_20131202."+op)
	creds, err := k.Creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("kinesis: credentials: %w", err)
	}
	awsv4.Sign(req, body, creds, k.Region, "kinesis", time.Now().UTC())
	hc := k.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Status: resp.StatusCode, Type: e.Type, Msg: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// PutRecord writes one record.
func (k *Kinesis) PutRecord(ctx context.Context, streamName, partitionKey string, data []byte) error {
	return k.call(ctx, "PutRecord", map[string]any{"StreamName": streamName, "PartitionKey": partitionKey, "Data": data}, nil)
}

// Shard is a Kinesis shard.
type Shard struct {
	ShardID        string `json:"ShardId"`
	ParentShardID  string `json:"ParentShardId"`
	AdjacentParent string `json:"AdjacentParentShardId"`
}

// ListShards returns all shards of a stream.
func (k *Kinesis) ListShards(ctx context.Context, streamName string) ([]Shard, error) {
	var all []Shard
	in := map[string]any{"StreamName": streamName}
	for {
		var out struct {
			Shards    []Shard
			NextToken string
		}
		if err := k.call(ctx, "ListShards", in, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Shards...)
		if out.NextToken == "" {
			return all, nil
		}
		in = map[string]any{"NextToken": out.NextToken} // StreamName must be omitted with NextToken
	}
}

// ShardIterator returns an iterator after seq, or at the oldest record.
func (k *Kinesis) ShardIterator(ctx context.Context, streamName, shardID, afterSeq string) (string, error) {
	in := map[string]any{"StreamName": streamName, "ShardId": shardID, "ShardIteratorType": "TRIM_HORIZON"}
	if afterSeq != "" {
		in["ShardIteratorType"], in["StartingSequenceNumber"] = "AFTER_SEQUENCE_NUMBER", afterSeq
	}
	var out struct{ ShardIterator string }
	err := k.call(ctx, "GetShardIterator", in, &out)
	return out.ShardIterator, err
}

// Record is a Kinesis record.
type Record struct {
	SequenceNumber string
	PartitionKey   string
	Data           []byte
}

// Records is one GetRecords page. Next is "" when the shard is closed and
// fully read.
type Records struct {
	Records []Record
	Next    string
	Behind  int64
}

// GetRecords reads up to limit records.
func (k *Kinesis) GetRecords(ctx context.Context, iterator string, limit int) (Records, error) {
	var out struct {
		Records            []Record
		NextShardIterator  string
		MillisBehindLatest int64
	}
	err := k.call(ctx, "GetRecords", map[string]any{"ShardIterator": iterator, "Limit": limit}, &out)
	return Records{Records: out.Records, Next: out.NextShardIterator, Behind: out.MillisBehindLatest}, err
}
