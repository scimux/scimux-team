package team

import (
	"net/http"

	"github.com/scimux/scimux-team/internal/registry"
)

// revokeSeatFn is the registry call behind DELETE /v1/seat. A test replaces
// it to force the failure branch a single registry will not produce.
var revokeSeatFn = func(reg *registry.Registry, id string) (registry.Code, error) {
	return reg.RevokeSeat(id)
}

type nameReq struct {
	Name string `json:"name"`
}

func (s *server) putSeat(w http.ResponseWriter, r *http.Request) {
	var seat registry.Seat
	err := s.withRegistry(func() error {
		authed, ok := authenticate(s.reg, r)
		if !ok {
			return errUnauthorized
		}
		seat = authed
		return nil
	})
	if err != nil {
		s.finish(w, err, 0, nil)
		return
	}
	var req nameReq
	if !readJSON(w, r, &req) {
		return
	}
	err = s.withRegistry(func() error {
		authed, ok := authenticate(s.reg, r)
		if !ok {
			return errUnauthorized
		}
		if beforeRename != nil {
			beforeRename()
		}
		var renErr error
		seat, renErr = s.reg.Rename(authed.ID, req.Name)
		// Rename replays the journal and can apply a revoke before it
		// accepts or refuses the name. That seat is already inactive.
		if err := s.reconcile(nil); err != nil {
			s.noteCleanup(nil)
			return err
		}
		return renErr
	})
	s.finish(w, err, http.StatusOK, seatJSON{ID: seat.ID, Name: seat.Name})
}

func (s *server) deleteSeat(w http.ResponseWriter, r *http.Request) {
	err := s.withRegistry(func() error {
		seat, ok := authenticate(s.reg, r)
		if !ok {
			return errUnauthorized
		}
		if beforeRevokeSeat != nil {
			beforeRevokeSeat()
		}
		if _, err := revokeSeatFn(s.reg, seat.ID); err != nil {
			// RevokeSeat replays other revokes before it can fail. Those
			// seats are already inactive.
			if recErr := s.reconcile(nil); recErr != nil {
				s.noteCleanup(nil)
				return recErr
			}
			return err
		}
		if err := s.reconcile(nil); err != nil {
			s.noteCleanup(nil)
			return err
		}
		return nil
	})
	s.finish(w, err, http.StatusNoContent, nil)
}

func (s *server) deleteMembership(w http.ResponseWriter, r *http.Request) {
	owner, chat := r.PathValue("owner"), r.PathValue("chat")
	err := s.withRegistry(func() error {
		g, err := s.gate(r, owner, chat, needRead, false)
		if err != nil {
			return err
		}
		if g.seat.ID == owner {
			return errForbidden
		}
		return s.channels.RemoveMember(owner, chat, g.seat.ID)
	})
	s.finish(w, err, http.StatusNoContent, nil)
}
