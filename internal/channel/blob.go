package channel

import "os"

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

func (s *Store) RemoveMember(owner, chatID, seat string) error { return nil }

func (s *Store) PutBlob(owner, chatID, id string, data []byte) (bool, error) {
	return false, nil
}

func (s *Store) GetBlob(owner, chatID, id string) ([]byte, error) { return nil, nil }
