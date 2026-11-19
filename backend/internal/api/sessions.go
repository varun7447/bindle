package api

import (
	"net/http"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

const maxSessionRuns = 200

type sessionJSON struct {
	query.Session
	Feedback query.FeedbackSummary `json:"feedback"`
}

// feedbackBySession folds per-run feedback into the runs' sessions.
func feedbackBySession(runs []query.Run, fb map[string]query.FeedbackSummary) map[string]query.FeedbackSummary {
	out := map[string]query.FeedbackSummary{}
	sums := map[string]float64{}
	for _, r := range runs {
		f, ok := fb[r.TraceID]
		if !ok || r.SessionID == "" {
			continue
		}
		s := out[r.SessionID]
		s.Positive += f.Positive
		s.Negative += f.Negative
		s.Count += f.Count
		if f.Average != nil {
			sums[r.SessionID] += *f.Average * float64(f.Count)
		}
		out[r.SessionID] = s
	}
	for id, s := range out {
		if total, ok := sums[id]; ok && s.Count > 0 {
			v := total / float64(s.Count)
			s.Average = &v
			out[id] = s
		}
	}
	return out
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	agent := r.URL.Query().Get("agent")
	if len(agent) > maxFilterLen {
		badRequest(w, "agent is too long")
		return
	}
	tenant := tenantFrom(r.Context())
	sessions, err := s.Store.ListSessions(r.Context(), tenant, query.SessionFilter{From: from, To: to, Agent: agent, Limit: 100})
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	fb, err := s.sessionFeedback(r, from, to)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	out := make([]sessionJSON, 0, len(sessions))
	for _, se := range sessions {
		out = append(out, sessionJSON{Session: se, Feedback: fb[se.SessionID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "sessions": out})
}

// sessionFeedback is user feedback in a window, per session.
func (s *Server) sessionFeedback(r *http.Request, from, to time.Time) (map[string]query.FeedbackSummary, error) {
	tenant := tenantFrom(r.Context())
	fb, err := s.Store.RecentFeedback(r.Context(), tenant, from, to.Add(time.Hour))
	if err != nil || len(fb) == 0 {
		return map[string]query.FeedbackSummary{}, err
	}
	ids := make([]string, 0, len(fb))
	for id := range fb {
		ids = append(ids, id)
	}
	runs, err := s.Store.RunsByIDs(r.Context(), tenant, ids)
	if err != nil {
		return nil, err
	}
	return feedbackBySession(runs, fb), nil
}

type turnJSON struct {
	Run      RunJSON                `json:"run"`
	Input    string                 `json:"input"`
	Output   string                 `json:"output"`
	Feedback *query.FeedbackSummary `json:"feedback,omitempty"`
}

// getSession returns a conversation: its runs in order, each with the
// user's input, the agent's answer and any feedback.
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > maxFilterLen || !utf8.ValidString(id) {
		badRequest(w, "invalid session ID")
		return
	}
	tenant := tenantFrom(r.Context())
	to := s.now()
	runs, err := s.Store.ListRuns(r.Context(), tenant, query.RunFilter{From: to.Add(-maxRange), To: to.Add(time.Minute), SessionID: id, Limit: maxSessionRuns})
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if len(runs) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no runs in this session in the last 30 days")
		return
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started.Before(runs[j].Started.Time) })
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.TraceID)
	}
	ios, err := s.Store.RunsForEval(r.Context(), tenant, ids)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	io := map[string][2]string{}
	for _, e := range ios {
		io[e.TraceID] = [2]string{preview(e.Input), preview(e.Output)}
	}
	fb, err := s.Store.Feedback(r.Context(), tenant, ids)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	turns := make([]turnJSON, 0, len(runs))
	var total query.Session
	for _, run := range runs {
		t := turnJSON{Run: runJSON(run), Input: io[run.TraceID][0], Output: io[run.TraceID][1]}
		if f, ok := fb[run.TraceID]; ok {
			f := f
			t.Feedback = &f
		}
		turns = append(turns, t)
		total.CostUSD += run.CostUSD
		total.DurationMs += run.DurationMs
		if run.CostUnknownSteps > 0 {
			total.CostPartial = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": id, "agent": runs[len(runs)-1].Agent, "turns": turns, "truncated": len(runs) == maxSessionRuns,
		"cost_usd": total.CostUSD, "cost_partial": total.CostPartial, "duration_ms": total.DurationMs,
		"feedback": feedbackBySession(runs, fb)[id],
	})
}

const maxSearchQuery = 200

// search is full-text search over step content and errors.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := q.Get("q")
	if l := len([]rune(text)); l == 0 || l > maxSearchQuery || !utf8.ValidString(text) {
		badRequest(w, "q must be 1 to 200 characters")
		return
	}
	name, from, to, ok := s.window(w, r, "7d")
	if !ok {
		return
	}
	agent, typ, status := q.Get("agent"), q.Get("type"), q.Get("status")
	if len(agent) > maxFilterLen || len(typ) > 32 || (status != "" && status != "failure") {
		badRequest(w, "invalid filter")
		return
	}
	hits, err := s.Store.Search(r.Context(), tenantFrom(r.Context()), query.SearchFilter{Query: text, From: from, To: to, Agent: agent, Type: typ, Status: status, Limit: 50})
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": name, "q": text, "hits": hits})
}
