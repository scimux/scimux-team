package team

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/scimux/scimux-team/internal/channel"
	"github.com/scimux/scimux-team/internal/registry"
)

// Access levels. A higher number includes every lower one.
const (
	accessNone = 0
	needRead   = 1
	needWrite  = 2
	needOwner  = 3
)

// timeLayout matches the protocol. It is duplicated because channel's
// layout is unexported; both format through UnixMilli.
const timeLayout = "2006-01-02T15:04:05.000Z"

// errUnknownMember is a member list that names a seat the registry does
// not have. Structural problems are channel.ErrInvalid instead.
var errUnknownMember = errors.New("unknown member")

// errNoChannels is a handler that was built without a store.
var errNoChannels = errors.New("channels unavailable")

type channelView struct {
	Owner        string            `json:"owner"`
	Chat         string            `json:"chat"`
	Title        string            `json:"title"`
	Generation   string            `json:"generation"`
	CreatedAt    string            `json:"created_at"`
	Access       string            `json:"access"`
	MirrorLast   uint64            `json:"mirror_last"`
	CommentsLast uint64            `json:"comments_last"`
	Members      *[]channel.Member `json:"members,omitempty"`
}

type channelsResp struct {
	Channels []channelView `json:"channels"`
}

type titleReq struct {
	Title string `json:"title"`
}

type membersReq struct {
	Members *[]channel.Member `json:"members"`
}

type appendReq struct {
	Entries []channel.NewEntry `json:"entries"`
}

type appendResp struct {
	Generation string           `json:"generation"`
	Results    []channel.Result `json:"results"`
	Last       uint64           `json:"last"`
}

type readResp struct {
	Generation string          `json:"generation"`
	Entries    []channel.Entry `json:"entries"`
	Last       uint64          `json:"last"`
	More       bool            `json:"more"`
}

type gateResult struct {
	seat registry.Seat
	info channel.Info
}

func (s *server) listChannels(w http.ResponseWriter, r *http.Request) {
	views := []channelView{}
	err := s.withRegistry(func() error {
		// GET patterns also match HEAD. This server does not list HEAD.
		if r.Method != http.MethodGet {
			return errNotFound
		}
		g, err := s.gate(r, "", "", needRead, false)
		if err != nil {
			return err
		}
		all, err := s.channels.List(g.seat.ID)
		if err != nil {
			return err
		}
		for _, info := range all {
			views = append(views, channelViewOf(info, accessName(accessOf(g.seat.ID, info))))
		}
		return nil
	})
	s.finish(w, err, http.StatusOK, channelsResp{Channels: views})
}

func (s *server) putChannel(w http.ResponseWriter, r *http.Request) {
	owner, chat := r.PathValue("owner"), r.PathValue("chat")
	if !s.allow(w, r, owner, chat, needOwner, true) {
		return
	}
	var req titleReq
	if !readJSON(w, r, &req) {
		return
	}
	var view channelView
	status := http.StatusOK
	err := s.withRegistry(func() error {
		if _, err := s.gate(r, owner, chat, needOwner, true); err != nil {
			return err
		}
		info, created, err := s.channels.Put(owner, chat, req.Title)
		if err != nil {
			return err
		}
		if created {
			status = http.StatusCreated
		}
		view = channelViewOf(info, "owner")
		return nil
	})
	s.finish(w, err, status, view)
}

func (s *server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	owner, chat := r.PathValue("owner"), r.PathValue("chat")
	err := s.withRegistry(func() error {
		if _, err := s.gate(r, owner, chat, needOwner, false); err != nil {
			return err
		}
		return s.channels.Delete(owner, chat)
	})
	s.finish(w, err, http.StatusNoContent, nil)
}

func (s *server) putMembers(w http.ResponseWriter, r *http.Request) {
	owner, chat := r.PathValue("owner"), r.PathValue("chat")
	if !s.allow(w, r, owner, chat, needOwner, false) {
		return
	}
	var req membersReq
	if !readJSON(w, r, &req) {
		return
	}
	var view channelView
	err := s.withRegistry(func() error {
		if _, err := s.gate(r, owner, chat, needOwner, false); err != nil {
			return err
		}
		if req.Members == nil {
			return channel.ErrInvalid
		}
		if err := screenMembers(s.reg, owner, *req.Members); err != nil {
			return err
		}
		info, err := s.channels.SetMembers(owner, chat, *req.Members)
		// A failure here is a store error. The view is discarded by finish.
		view = channelViewOf(info, "owner")
		return err
	})
	s.finish(w, err, http.StatusOK, view)
}

func (s *server) getMirror(w http.ResponseWriter, r *http.Request) {
	s.getLog(w, r, channel.Mirror)
}

func (s *server) getComments(w http.ResponseWriter, r *http.Request) {
	s.getLog(w, r, channel.Comments)
}

func (s *server) postMirror(w http.ResponseWriter, r *http.Request) {
	s.postLog(w, r, channel.Mirror, needOwner)
}

func (s *server) postComments(w http.ResponseWriter, r *http.Request) {
	s.postLog(w, r, channel.Comments, needWrite)
}

func (s *server) getLog(w http.ResponseWriter, r *http.Request, kind channel.Log) {
	owner, chat := r.PathValue("owner"), r.PathValue("chat")
	var resp readResp
	err := s.withRegistry(func() error {
		if r.Method != http.MethodGet {
			return errNotFound
		}
		g, err := s.gate(r, owner, chat, needRead, false)
		if err != nil {
			return err
		}
		after, err := parseAfter(r)
		if err != nil {
			return err
		}
		entries, last, more, err := s.channels.Read(owner, chat, kind, after)
		resp = readResp{Generation: g.info.Generation, Entries: entries, Last: last, More: more}
		return err
	})
	s.finish(w, err, http.StatusOK, resp)
}

func (s *server) postLog(w http.ResponseWriter, r *http.Request, kind channel.Log, need int) {
	owner, chat := r.PathValue("owner"), r.PathValue("chat")
	if !s.allow(w, r, owner, chat, need, false) {
		return
	}
	var req appendReq
	if !readJSON(w, r, &req) {
		return
	}
	var resp appendResp
	err := s.withRegistry(func() error {
		g, err := s.gate(r, owner, chat, need, false)
		if err != nil {
			return err
		}
		results, last, err := s.channels.Append(owner, chat, kind, g.seat.ID, req.Entries)
		if err != nil {
			return err
		}
		resp = appendResp{Generation: g.info.Generation, Results: results, Last: last}
		return nil
	})
	s.finish(w, err, http.StatusOK, resp)
}

// allow answers the request and reports whether the caller may continue
// to read the body. Authentication, visibility and permission run here,
// before any byte of the body is decoded.
func (s *server) allow(w http.ResponseWriter, r *http.Request, owner, chat string, need int, allowCreate bool) bool {
	err := s.withRegistry(func() error {
		_, err := s.gate(r, owner, chat, need, allowCreate)
		return err
	})
	if err != nil {
		s.finish(w, err, 0, nil)
		return false
	}
	return true
}

// reconcile drops channels of inactive owners and inactive members.
// skip lists seats that Register just rebound; they are active and must
// still lose what the previous holder owned.
func (s *server) reconcile(skip []string) error {
	if s.channels == nil {
		return nil
	}
	return s.channels.Reconcile(func(id string) bool {
		if _, ok := s.reg.Seat(id); !ok {
			return false
		}
		return !containsID(skip, id)
	})
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// gate decides what the caller may do. A stranger and a missing channel
// are both errNotFound. allowCreate lets the path owner create a channel
// that is not there yet; every other missing channel is not found.
func (s *server) gate(r *http.Request, owner, chat string, need int, allowCreate bool) (gateResult, error) {
	seat, ok := authenticate(s.reg, r)
	if !ok {
		return gateResult{}, errUnauthorized
	}
	if s.channels == nil {
		return gateResult{}, errNoChannels
	}
	if owner == "" {
		return gateResult{seat: seat}, nil
	}
	if !validID(owner) || !validChatID(chat) {
		return gateResult{}, channel.ErrInvalid
	}
	info, err := s.channels.Get(owner, chat)
	if errors.Is(err, channel.ErrNotFound) {
		if allowCreate && seat.ID == owner {
			return gateResult{seat: seat}, nil
		}
		return gateResult{}, errNotFound
	}
	if err != nil {
		return gateResult{}, err
	}
	acc := accessOf(seat.ID, info)
	if acc == accessNone {
		return gateResult{}, errNotFound
	}
	if acc < need {
		return gateResult{}, errForbidden
	}
	return gateResult{seat: seat, info: info}, nil
}

func accessOf(id string, info channel.Info) int {
	if id == info.Owner {
		return needOwner
	}
	for _, m := range info.Members {
		if m.Seat != id {
			continue
		}
		if m.Access == "write" {
			return needWrite
		}
		return needRead
	}
	return accessNone
}

func accessName(acc int) string {
	switch acc {
	case needOwner:
		return "owner"
	case needWrite:
		return "write"
	default:
		return "read"
	}
}

func channelViewOf(info channel.Info, access string) channelView {
	v := channelView{
		Owner:        info.Owner,
		Chat:         info.Chat,
		Title:        info.Title,
		Generation:   info.Generation,
		CreatedAt:    formatStamp(info.Created),
		Access:       access,
		MirrorLast:   info.MirrorLast,
		CommentsLast: info.CommentsLast,
	}
	if access == "owner" {
		members := make([]channel.Member, len(info.Members))
		copy(members, info.Members)
		v.Members = &members
	}
	return v
}

func formatStamp(t time.Time) string {
	return time.UnixMilli(t.UTC().UnixMilli()).UTC().Format(timeLayout)
}

func parseAfter(r *http.Request) (uint64, error) {
	vals, ok := r.URL.Query()["after"]
	if !ok {
		return 0, nil
	}
	if len(vals) != 1 || vals[0] == "" {
		return 0, channel.ErrInvalid
	}
	n, err := strconv.ParseUint(vals[0], 10, 64)
	if err != nil {
		return 0, channel.ErrInvalid
	}
	return n, nil
}

// screenMembers applies the protocol's structural rules before it asks the
// registry whether each seat exists. An unknown seat is reported only once
// the list itself is well formed.
func screenMembers(reg *registry.Registry, owner string, members []channel.Member) error {
	if len(members) > 200 {
		return channel.ErrInvalid
	}
	seen := map[string]bool{}
	for _, m := range members {
		if !validID(m.Seat) || m.Seat == owner || (m.Access != "read" && m.Access != "write") || seen[m.Seat] {
			return channel.ErrInvalid
		}
		seen[m.Seat] = true
		if _, ok := reg.Seat(m.Seat); !ok {
			return errUnknownMember
		}
	}
	return nil
}

func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validChatID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
