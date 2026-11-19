package team

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/scimux/scimux-team/internal/channel"
)

// An obsolete upload must not publish into a newly created generation.
func TestCoordinatorUploadCannotCrossGeneration(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "synthetic owner")
	p.create(t, owner, ownerID, "main", "synthetic original")
	data := []byte("synthetic delayed bytes")
	base := "/v1/channels/" + ownerID + "/main"
	path := base + "/blobs/" + blobID(data) + "?generation=" + gen1
	pr, pw := io.Pipe()
	t.Cleanup(func() { pr.Close(); pw.Close() })
	entered := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		status, _, _ := do(p.h, http.MethodPut, path, owner, &signalReader{r: pr, entered: entered})
		done <- status
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("body not reached")
	}
	status, _, _ := call(p.h, http.MethodDelete, base+"?generation="+gen1, owner, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete status %d", status)
	}
	p.create(t, owner, ownerID, "main", "synthetic replacement")
	if _, err := pw.Write(data); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	select {
	case status = <-done:
	case <-time.After(time.Second):
		t.Fatal("upload not finished")
	}
	if status != http.StatusConflict {
		t.Errorf("obsolete generation upload status %d; want conflict", status)
	}
	status, _, _ = call(p.h, http.MethodGet, base+"/blobs/"+blobID(data), owner, "")
	if status != http.StatusNotFound {
		t.Errorf("old upload bytes reached replacement: GET status %d", status)
	}
}

// Capability discovery must leave the strict legacy hello body unchanged.
func TestPeopleBlobGenerationCapabilityOnHello(t *testing.T) {
	p := newParty(t, channel.Options{})
	status, headers, _ := do(p.h, http.MethodGet, "/v1/hello", "", nil)
	if status != http.StatusOK || headers.Get("Scimux-Blob-Generation") != "1" {
		t.Fatalf("generation-safe blob capability: status %d header %q", status, headers.Get("Scimux-Blob-Generation"))
	}
}

func TestPeopleBlobGenerationGuardValidation(t *testing.T) {
	p := newParty(t, channel.Options{})
	ownerID := inst(0x11)
	owner := p.seat(t, 0x11, "synthetic owner")
	stranger := p.seat(t, 0x22, "synthetic stranger")
	p.create(t, owner, ownerID, "main", "synthetic channel")
	payload := []byte("synthetic payload")
	base := "/v1/channels/" + ownerID + "/main/blobs/" + blobID(payload)
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		for _, tc := range []struct {
			query string
			want  int
		}{
			{"?generation=bad", http.StatusBadRequest},
			{"?generation=%ZZ", http.StatusBadRequest},
			{"?generation=" + gen1 + "&generation=" + gen1, http.StatusBadRequest},
			{"?generation=" + gen2, http.StatusConflict},
		} {
			status, _, _ := call(p.h, method, base+tc.query, owner, string(payload))
			if status != tc.want {
				t.Errorf("%s %s: status %d want %d", method, tc.query, status, tc.want)
			}
		}
		status, _, _ := call(p.h, method, base+"?generation=bad", stranger, string(payload))
		if status != http.StatusNotFound {
			t.Errorf("%s unauthorized must precede generation validation: %d", method, status)
		}
	}
}
