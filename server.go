package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

//
// TYPES
//

// Mock is one canned answer read from mocks.json.
//
// Body and Sequence are mutually exclusive: Sequence cycles one entry per
// call, Body always answers the same thing.
type Mock struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Status   int             `json:"status,omitempty"`
	DelayMS  int             `json:"delayMs,omitempty"`
	Body     json.RawMessage `json:"body,omitempty"`
	Sequence []SequenceStep  `json:"sequence,omitempty"`
}

// SequenceStep is one answer of a cycling sequence.
type SequenceStep struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// Request is one captured call: a row in the TUI and a line in the log file.
type Request struct {
	Time     time.Time           `json:"time"`
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	Query    string              `json:"query,omitempty"`
	Headers  map[string][]string `json:"headers"`
	Body     string              `json:"body,omitempty"`
	Status   int                 `json:"status"`
	Endpoint string              `json:"endpoint"`
}

// Server answers webhook calls from mocks.json and hands every call to Sink.
type Server struct {
	MocksFile   string
	Secret      string
	LogFile     string
	StatusCodes []int

	// Sink receives every captured request, set by main to feed the TUI.
	Sink func(Request)

	mu        sync.Mutex
	patterns  map[string]*regexp.Regexp // compiled globs, keyed by raw pattern
	sequences map[string]int            // sequence cursor, keyed by method+path
	overrides map[string]int            // forced status, keyed by endpoint
}

//
// CONSTANTS
//

// unmatched names the screen collecting calls that no mock answered.
const unmatched = "(unmatched)"

// escapes matches control characters. A captured request comes from the
// network and its text is printed to a terminal, so it must never carry its
// own ANSI sequences.
var escapes = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)

//
// CODE
//

// NewServer returns a Server ready to answer calls.
func NewServer(mocksFile, secret, logFile string, codes []int) *Server {
	return &Server{
		MocksFile:   mocksFile,
		Secret:      secret,
		LogFile:     logFile,
		StatusCodes: codes,
		patterns:    map[string]*regexp.Regexp{},
		sequences:   map[string]int{},
		overrides:   map[string]int{},
	}
}

// clean replaces control characters so untrusted text is safe on a terminal.
func clean(text string) string {
	return escapes.ReplaceAllString(text, "?")
}

// LoadMocks re-reads mocks.json, so editing it needs no restart.
//
// Returns an error when the file is missing or is not valid JSON.
func (s *Server) LoadMocks() ([]Mock, error) {
	raw, err := os.ReadFile(s.MocksFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.MocksFile, err)
	}

	var mocks []Mock
	if err := json.Unmarshal(raw, &mocks); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.MocksFile, err)
	}

	return mocks, nil
}

// glob compiles a mocks.json path pattern, caching the result.
//
// Unlike path.Match, * crosses slashes here, which is what Python's fnmatch
// did and what a pattern like /meta/*/messages relies on.
func (s *Server) glob(pattern string) *regexp.Regexp {
	s.mu.Lock()
	defer s.mu.Unlock()

	// already compiled: reuse it
	if compiled, ok := s.patterns[pattern]; ok {
		return compiled
	}

	// QuoteMeta escapes everything, so the result is only literals and .*
	// and can never fail to compile
	quoted := regexp.QuoteMeta(pattern)
	compiled := regexp.MustCompile("^" + strings.ReplaceAll(quoted, `\*`, `.*`) + "$")

	s.patterns[pattern] = compiled
	return compiled
}

// findMock returns the first mock answering method and path, nil when none do.
func (s *Server) findMock(method, path string) *Mock {
	mocks, err := s.LoadMocks()
	if err != nil {
		// unreadable mocks file: every call falls through to the fallback
		return nil
	}

	for _, mock := range mocks {
		// wrong verb: skip
		if mock.Method != method {
			continue
		}

		// path matches: first mock wins
		if s.glob(mock.Path).MatchString(path) {
			return &mock
		}
	}

	return nil
}

// Endpoints returns the screen name of every mock, in file order.
func (s *Server) Endpoints() []string {
	mocks, err := s.LoadMocks()
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(mocks))
	for _, mock := range mocks {
		names = append(names, endpointName(&mock))
	}

	return names
}

// SetOverride forces every answer of an endpoint to status, 0 clears it.
func (s *Server) SetOverride(endpoint string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// zero means no override: drop the entry
	if status == 0 {
		delete(s.overrides, endpoint)
		return
	}

	s.overrides[endpoint] = status
}

// endpointName returns the screen a mock logs to.
func endpointName(mock *Mock) string {
	return mock.Method + " " + mock.Path
}

// answer picks the status and body of a call, advancing the sequence cursor.
func (s *Server) answer(mock *Mock, endpoint string) (int, json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	status, body := 0, json.RawMessage(nil)

	// mock configured: take its status and body
	if mock != nil {
		status, body = mock.Status, mock.Body
	}

	// sequence configured: it wins and advances one step per call
	if mock != nil && len(mock.Sequence) > 0 {
		key := endpointName(mock)
		step := mock.Sequence[s.sequences[key]%len(mock.Sequence)]
		s.sequences[key]++
		status, body = step.Status, step.Body
	}

	// nothing said otherwise: draw from the configured status codes
	if status == 0 {
		status = s.StatusCodes[rand.Intn(len(s.StatusCodes))]
	}

	// override set from the TUI: it beats everything
	if forced, ok := s.overrides[endpoint]; ok {
		status = forced
	}

	return status, body
}

// strip removes the secret prefix, reporting whether the call may be served.
func (s *Server) strip(path string) (string, bool) {
	// no secret: every path is open, fine for localhost
	if s.Secret == "" {
		return path, true
	}

	prefix := "/" + s.Secret + "/"

	// wrong prefix: scanners get nothing, not even a log line
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}

	return "/" + strings.TrimPrefix(path, prefix), true
}

// appendLog writes one JSON line per call, so a session survives a restart.
func (s *Server) appendLog(req Request) {
	// no log file configured: nothing to do
	if s.LogFile == "" {
		return
	}

	line, err := json.Marshal(req)
	if err != nil {
		return
	}

	file, err := os.OpenFile(s.LogFile,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)

	// log file unavailable: the TUI still has the request, drop the line
	if err != nil {
		return
	}
	defer file.Close()

	// the request is already in the TUI: a failed write is not actionable
	_, _ = file.Write(append(line, '\n'))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, allowed := s.strip(r.URL.Path)

	// secret missing: silent 404
	if !allowed {
		http.Error(w, `{"detail":"Not Found"}`, http.StatusNotFound)
		return
	}

	// the reader is closed by net/http after the handler returns
	body, _ := io.ReadAll(r.Body)

	mock := s.findMock(r.Method, path)
	endpoint := unmatched

	// mock matched: the call belongs to that mock's screen
	if mock != nil {
		endpoint = endpointName(mock)
	}

	status, mockBody := s.answer(mock, endpoint)

	s.capture(Request{
		Time:     time.Now(),
		Method:   r.Method,
		Path:     clean(path),
		Query:    clean(r.URL.RawQuery),
		Headers:  r.Header,
		Body:     string(body),
		Status:   status,
		Endpoint: endpoint,
	})

	// delay configured: hold the answer to exercise client timeouts
	if mock != nil && mock.DelayMS > 0 {
		time.Sleep(time.Duration(mock.DelayMS) * time.Millisecond)
	}

	// no mock body: echo back what was received
	if mockBody == nil {
		mockBody = json.RawMessage(fmt.Sprintf(
			`{"status":"received","method":%q,"path":%q}`, r.Method, path))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(mockBody)
}

// capture logs a request to disk and hands it to the TUI.
func (s *Server) capture(req Request) {
	s.appendLog(req)

	// no sink yet: the TUI is not running
	if s.Sink == nil {
		return
	}

	s.Sink(req)
}
