package team

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/scimux/scimux-team/internal/channel"
)

// maxBlobBytes matches the channel store. It is duplicated because that
// constant is unexported.
const maxBlobBytes = 4 << 20

type blobResp struct {
	ID   string `json:"id"`
	Size int    `json:"size"`
}

func (s *server) putBlob(w http.ResponseWriter, r *http.Request) {
	owner, chat, id := r.PathValue("owner"), r.PathValue("chat"), r.PathValue("id")
	if !s.allow(w, r, owner, chat, needWrite, false) {
		return
	}
	if !validBlobID(id) {
		s.finish(w, channel.ErrInvalid, 0, nil)
		return
	}
	data, err := readBlobBody(r.Body)
	if err != nil {
		s.finish(w, err, 0, nil)
		return
	}
	var created bool
	err = s.withRegistry(func() error {
		if _, err := s.gate(r, owner, chat, needWrite, false); err != nil {
			return err
		}
		var putErr error
		created, putErr = s.channels.PutBlob(owner, chat, id, data)
		return putErr
	})
	status := http.StatusOK
	if err == nil && created {
		status = http.StatusCreated
	}
	s.finish(w, err, status, blobResp{ID: id, Size: len(data)})
}

func (s *server) getBlob(w http.ResponseWriter, r *http.Request) {
	owner, chat, id := r.PathValue("owner"), r.PathValue("chat"), r.PathValue("id")
	var data []byte
	err := s.withRegistry(func() error {
		// GET patterns also match HEAD. This server does not answer HEAD.
		if r.Method != http.MethodGet {
			return errNotFound
		}
		if _, err := s.gate(r, owner, chat, needRead, false); err != nil {
			return err
		}
		if !validBlobID(id) {
			return channel.ErrInvalid
		}
		got, err := s.channels.GetBlob(owner, chat, id)
		if errors.Is(err, channel.ErrNotFound) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		data = append([]byte(nil), got...)
		return nil
	})
	if err != nil {
		s.finish(w, err, 0, nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func readBlobBody(r io.Reader) ([]byte, error) {
	buf := make([]byte, maxBlobBytes+1)
	n, err := io.ReadFull(r, buf)
	if err == io.EOF && n == 0 {
		return nil, channel.ErrInvalid
	}
	if err == io.ErrUnexpectedEOF {
		return buf[:n], nil
	}
	if err == nil {
		return nil, channel.ErrTooLarge
	}
	return nil, err
}

func validBlobID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
