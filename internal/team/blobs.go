package team

import (
	"errors"
	"io"
	"net/http"
	"net/url"
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
	if err := s.withRegistry(func() error {
		g, err := s.gate(r, owner, chat, needWrite, false)
		if err != nil {
			return err
		}
		return blobGeneration(r, g.info.Generation)
	}); err != nil {
		s.finish(w, err, 0, nil)
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
		g, err := s.gate(r, owner, chat, needWrite, false)
		if err != nil {
			return err
		}
		if err := blobGeneration(r, g.info.Generation); err != nil {
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
		g, err := s.gate(r, owner, chat, needRead, false)
		if err != nil {
			return err
		}
		if err := blobGeneration(r, g.info.Generation); err != nil {
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

// blobGeneration accepts legacy unguarded calls and fences guarded calls.
// The caller holds the registry serialization lock and has already checked access.
func blobGeneration(r *http.Request, current string) error {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return channel.ErrInvalid
	}
	values, present := query["generation"]
	if !present {
		return nil
	}
	if len(values) != 1 || !validGeneration(values[0]) {
		return channel.ErrInvalid
	}
	if values[0] != current {
		return errStaleGeneration
	}
	return nil
}
