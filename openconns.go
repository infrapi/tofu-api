package main

import (
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// openConns follows the server's connections and the request each one
// serves, to say at shutdown what is still active.
type openConns struct {
	next  http.Handler
	mu    sync.Mutex
	conns map[string]*openConn
}

type openConn struct {
	state   http.ConnState
	request string
	start   time.Time
}

type activeConn struct {
	remote, request, since string
}

func newOpenConns(next http.Handler) *openConns {
	return &openConns{next: next, conns: map[string]*openConn{}}
}

func (o *openConns) track(c net.Conn, state http.ConnState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := c.RemoteAddr().String()
	switch state {
	case http.StateClosed, http.StateHijacked:
		delete(o.conns, key)
		return
	case http.StateNew, http.StateActive, http.StateIdle:
	}
	oc, ok := o.conns[key]
	if !ok {
		oc = &openConn{}
		o.conns[key] = oc
	}
	oc.state = state
	if state != http.StateActive {
		oc.request = ""
	}
	oc.start = time.Now()
}

func (o *openConns) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	desc := r.Method + " " + r.URL.Path + " (" + r.UserAgent() + ")"
	o.mu.Lock()
	if oc, ok := o.conns[r.RemoteAddr]; ok {
		oc.request = desc
		oc.start = time.Now()
	}
	o.mu.Unlock()
	o.next.ServeHTTP(w, r)
}

func (o *openConns) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.conns)
}

// active are the connections not idle: serving a request, or opened and
// not sent one yet.
func (o *openConns) active() []activeConn {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []activeConn
	for remote, oc := range o.conns {
		if oc.state == http.StateIdle {
			continue
		}
		req := oc.request
		if req == "" {
			req = "(none yet: " + oc.state.String() + ")"
		}
		out = append(out, activeConn{remote: remote, request: req, since: time.Since(oc.start).Round(time.Millisecond).String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].remote < out[j].remote })
	return out
}
