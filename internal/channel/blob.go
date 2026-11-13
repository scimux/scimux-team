package channel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	maxBlobBytes        = 4 << 20
	defaultMaxBlobBytes = 256 << 20
	defaultMaxBlobs     = 4096
	blobsName           = "blobs"
)

// Blob files are opened through these seams so a test can count them and
// force a failure a local disk will not produce.
var (
	openBlob  = os.Open
	closeBlob = func(f *os.File) error { return f.Close() }
	lstatPath = os.Lstat
)

// RemoveMember drops one seat from the member list and keeps the order of
// the rest. A seat that is not a member is not found.
func (s *Store) RemoveMember(owner, chatID, seat string) error {
	if !validOwner(owner) || !validChat(chatID) || !validOwner(seat) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	ch, err := s.chatFor(owner, chatID)
	if err != nil {
		return err
	}
	next := make([]Member, 0, len(ch.meta.Members))
	found := false
	for _, mem := range ch.meta.Members {
		if mem.Seat == seat {
			found = true
			continue
		}
		next = append(next, mem)
	}
	if !found {
		return ErrNotFound
	}
	updated := *ch.meta
	updated.Members = next
	return s.saveMeta(ch, &updated)
}

// PutBlob stores data under its SHA-256 name. created is false when those
// bytes are already stored. A different body already at that name is damage
// and is left as it is.
func (s *Store) PutBlob(owner, chatID, id string, data []byte) (bool, error) {
	if !validOwner(owner) || !validChat(chatID) || !validBlobID(id) {
		return false, ErrInvalid
	}
	if len(data) == 0 {
		return false, ErrInvalid
	}
	if int64(len(data)) > maxBlobBytes {
		return false, ErrTooLarge
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != id {
		return false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return false, err
	}
	ch, err := s.chatFor(owner, chatID)
	if err != nil {
		return false, err
	}
	dir := filepath.Join(ch.dir, blobsName)
	existing, err := readStoredBlob(filepath.Join(dir, id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err == nil {
		if bytes.Equal(existing, data) {
			return false, nil
		}
		return false, errDamaged
	}
	if err := mkdirAll(dir, dirPerm); err != nil {
		return false, err
	}
	if err := chmodPath(dir, dirPerm); err != nil {
		return false, err
	}
	n, used, err := blobUsage(dir)
	if err != nil {
		return false, err
	}
	if n >= s.opts.MaxBlobsPerChannel || used+int64(len(data)) > s.opts.MaxBlobBytes {
		return false, ErrQuota
	}
	if err := writeAtomic(dir, id, data); err != nil {
		return false, err
	}
	return true, nil
}

// GetBlob returns one blob. Damage here harms only that blob: the channel
// and its other blobs stay usable, and repairing the file clears it.
func (s *Store) GetBlob(owner, chatID, id string) ([]byte, error) {
	if !validOwner(owner) || !validChat(chatID) || !validBlobID(id) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	ch, err := s.chatFor(owner, chatID)
	if err != nil {
		return nil, err
	}
	data, err := readStoredBlob(filepath.Join(ch.dir, blobsName, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if blobDamaged(id, data) {
		return nil, errDamaged
	}
	return data, nil
}

func validBlobID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !hexDigit(id[i]) {
			return false
		}
	}
	return true
}

func blobDamaged(id string, data []byte) bool {
	if len(data) == 0 || int64(len(data)) > maxBlobBytes {
		return true
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) != id
}

func blobUsage(dir string) (int, int64, error) {
	entries, err := readDir(dir)
	if err != nil {
		return 0, 0, err
	}
	var n int
	var used int64
	for _, e := range entries {
		if !validBlobID(e.Name()) {
			continue
		}
		info, err := lstatPath(filepath.Join(dir, e.Name()))
		if err != nil {
			return 0, 0, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		n++
		used += info.Size()
	}
	return n, used, nil
}

// readStoredBlob returns the blob bytes. A missing file is os.ErrNotExist.
// Anything that is not a regular file is damage. The file is closed before
// return, including on a read failure.
func readStoredBlob(path string) ([]byte, error) {
	info, err := lstatPath(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errDamaged
	}
	f, err := openBlob(path)
	if err != nil {
		return nil, err
	}
	data, rerr := readFull(io.LimitReader(f, maxBlobBytes+1))
	_ = closeBlob(f)
	if rerr != nil {
		return nil, rerr
	}
	return data, nil
}
