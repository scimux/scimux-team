package team

import (
	"net/http"

	"github.com/scimux/scimux-team/internal/channel"
)

// readResp is the read response. The stub exists so the tests compile.
type readResp struct {
	Generation string          `json:"generation"`
	Entries    []channel.Entry `json:"entries"`
	Last       uint64          `json:"last"`
	More       bool            `json:"more"`
}

func (s *server) listChannels(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) }

func (s *server) putChannel(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) }

func (s *server) deleteChannel(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) }

func (s *server) putMembers(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) }

func (s *server) getMirror(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) }

func (s *server) getComments(w http.ResponseWriter, r *http.Request) {
	s.notFound(w, r)
}

func (s *server) postMirror(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) }

func (s *server) postComments(w http.ResponseWriter, r *http.Request) {
	s.notFound(w, r)
}
