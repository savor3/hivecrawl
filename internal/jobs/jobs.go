// Package jobs keeps background jobs (crawl, batch scrape, extract, agent)
// in memory for 24 hours, like Firecrawl's job results.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"hivecrawl/internal/scrape"
)

const (
	TTL     = 24 * time.Hour
	maxJobs = 200
)

// Status values match Firecrawl's.
const (
	Scraping   = "scraping"
	Processing = "processing"
	Completed  = "completed"
	Failed     = "failed"
	Cancelled  = "cancelled"
)

type Error struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	URL       string `json:"url"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error"`
}

type Job struct {
	ID      string
	Kind    string // crawl | batch_scrape | extract | agent
	URL     string
	Options any
	Created time.Time

	mu            sync.Mutex
	status        string
	total         int
	completed     int
	docs          []*scrape.Document
	errs          []Error
	robotsBlocked []string
	finished      time.Time
	err           string
	data          any
	cancel        context.CancelFunc
}

func (j *Job) SetTotal(n int) {
	j.mu.Lock()
	if n > j.total {
		j.total = n
	}
	j.mu.Unlock()
}

func (j *Job) AddDoc(d *scrape.Document) {
	j.mu.Lock()
	j.docs = append(j.docs, d)
	j.completed++
	if j.completed > j.total {
		j.total = j.completed
	}
	j.mu.Unlock()
}

func (j *Job) AddError(url, code, msg string) {
	j.mu.Lock()
	j.errs = append(j.errs, Error{ID: NewID(), Timestamp: time.Now().UTC().Format(time.RFC3339), URL: url, Code: code, Error: msg})
	j.mu.Unlock()
}

func (j *Job) AddRobotsBlocked(url string) {
	j.mu.Lock()
	j.robotsBlocked = append(j.robotsBlocked, url)
	j.mu.Unlock()
}

// SetData stores a job's result (extract, agent).
func (j *Job) SetData(v any) {
	j.mu.Lock()
	j.data = v
	j.mu.Unlock()
}

// Finish moves the job to a final status unless it was cancelled.
func (j *Job) Finish(status, errMsg string) {
	j.mu.Lock()
	if j.status != Cancelled {
		j.status, j.err = status, errMsg
	}
	j.finished = time.Now()
	j.mu.Unlock()
}

func (j *Job) Cancel() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != Scraping && j.status != Processing {
		return false
	}
	j.status = Cancelled
	j.finished = time.Now()
	if j.cancel != nil {
		j.cancel()
	}
	return true
}

func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status == Scraping || j.status == Processing
}

// Snapshot is a consistent view of a job.
type Snapshot struct {
	Status        string
	Total         int
	Completed     int
	Docs          []*scrape.Document
	Next          int // skip value for the next page; 0 = no more
	Errors        []Error
	RobotsBlocked []string
	Err           string
	Data          any
	Created       time.Time
	Finished      time.Time
	ExpiresAt     time.Time
}

// pageBytes caps one status page, like Firecrawl's 10 MiB.
const pageBytes = 10 << 20

// View returns docs starting at skip, at most limit (0 = no limit) and
// about 10 MiB, plus where the next page starts.
func (j *Job) View(skip, limit int) Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{Status: j.status, Total: j.total, Completed: j.completed, Err: j.err, Data: j.data,
		Errors: append([]Error(nil), j.errs...), RobotsBlocked: append([]string(nil), j.robotsBlocked...),
		Created: j.Created, Finished: j.finished, ExpiresAt: j.Created.Add(TTL)}
	if skip < 0 || skip > len(j.docs) {
		skip = len(j.docs)
	}
	size := 0
	i := skip
	for ; i < len(j.docs); i++ {
		if limit > 0 && i-skip >= limit {
			break
		}
		b, _ := json.Marshal(j.docs[i])
		if size+len(b) > pageBytes && i > skip {
			break
		}
		size += len(b)
		s.Docs = append(s.Docs, j.docs[i])
	}
	if i < len(j.docs) || (j.status == Scraping && i == len(j.docs) && i > skip) {
		s.Next = i
	}
	return s
}

type Store struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func NewStore() *Store {
	s := &Store{jobs: map[string]*Job{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			s.expire()
		}
	}()
	return s
}

// Create registers a running job; cancel stops its work.
func (s *Store) Create(kind, url string, options any, cancel context.CancelFunc) *Job {
	status := Scraping
	if kind == "extract" || kind == "agent" {
		status = Processing
	}
	j := &Job{ID: NewID(), Kind: kind, URL: url, Options: options, Created: time.Now(), status: status, cancel: cancel}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	s.evict()
	return j
}

func (s *Store) Get(id, kind string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || (kind != "" && j.Kind != kind) {
		return nil, false
	}
	return j, true
}

// Running lists running jobs of a kind, newest first.
func (s *Store) Running(kind string) []*Job {
	s.mu.Lock()
	var out []*Job
	for _, j := range s.jobs {
		if j.Kind == kind {
			out = append(out, j)
		}
	}
	s.mu.Unlock()
	out = filter(out, (*Job).Running)
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

func filter(js []*Job, keep func(*Job) bool) []*Job {
	out := js[:0]
	for _, j := range js {
		if keep(j) {
			out = append(out, j)
		}
	}
	return out
}

func (s *Store) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, j := range s.jobs {
		if time.Since(j.Created) > TTL && !j.Running() {
			delete(s.jobs, id)
		}
	}
}

// evict drops the oldest finished jobs beyond maxJobs (memory bound).
func (s *Store) evict() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) <= maxJobs {
		return
	}
	var done []*Job
	for _, j := range s.jobs {
		if !j.Running() {
			done = append(done, j)
		}
	}
	sort.Slice(done, func(a, b int) bool { return done[a].Created.Before(done[b].Created) })
	for _, j := range done {
		if len(s.jobs) <= maxJobs {
			break
		}
		delete(s.jobs, j.ID)
	}
}

// NewID is a random UUIDv4.
func NewID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
