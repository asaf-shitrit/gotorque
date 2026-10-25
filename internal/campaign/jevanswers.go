package campaign

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/asaf-shitrit/gotorque/internal/jev"
)

// answerBucket holds one entry per answered request, keyed by jev.Digest.
var answerBucket = []byte("answers")

// AnswerRecord is the cross-campaign record of Jev's answers (ADR 0042).
//
// Jev is asked a fixed set of yes/no questions about a function's source, and
// it does not answer them identically twice: on the recorded campaigns the
// same question about the same function moved by up to 0.13 between
// campaigns, and two gates sit inside that spread (the 0.5 probability floor
// and the half-sigma z threshold), so a target could be flagged in one
// campaign and not in the next on the same source. The campaign cache (ADR
// 0028) removes the repetition inside one campaign; this record removes it
// across campaigns, by keying the answer to the exact thing that was asked
// (jev.Digest: pinned build, state, question set) instead of to the campaign
// that asked it.
//
// The record lives outside every campaign directory, because it must outlive
// one: <user cache>/gotorque/jev-answers.db. It is content-addressed and never
// pruned, so it grows by one small entry per distinct request ever answered
// and deleting it is always safe -- the next campaign simply asks again.
type AnswerRecord struct {
	db   *bolt.DB
	path string
}

// AnswerRecordPath is where the record lives when the operator names no other
// path: beside the campaigns in the user cache directory.
func AnswerRecordPath() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache: %w", err)
	}
	return filepath.Join(cache, "gotorque", "jev-answers.db"), nil
}

// OpenAnswerRecord opens the record at path, creating it when it is absent. A
// record is a plain key-value file with no migrations, so a campaign that
// cannot open one (another gotorque process holds it, or the cache directory
// is not writable) runs without it rather than failing.
func OpenAnswerRecord(path string) (*AnswerRecord, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("jev answer record path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		// The only thing that holds this file's exclusive lock is another
		// gotorque process recording answers, so say that rather than leaving
		// a bare "timeout" that reads like a filesystem fault.
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("another gotorque process holds the Jev answer record %s: %w", path, err)
		}
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, createErr := tx.CreateBucketIfNotExists(answerBucket)
		return createErr
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &AnswerRecord{db: db, path: path}, nil
}

// Path is the file this record was opened from, for the campaign state and
// report.
func (r *AnswerRecord) Path() string { return r.path }

// Close releases the record. A campaign that recorded into it keeps working
// after this: the record is only ever consulted for a request that campaign's
// own cache does not already hold.
func (r *AnswerRecord) Close() error { return r.db.Close() }

// Get returns the answer already recorded for digest. A miss is reported by
// ok == false with a nil error.
func (r *AnswerRecord) Get(digest string) (jev.Response, bool, error) {
	var resp jev.Response
	found := false
	err := r.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(answerBucket).Get([]byte(digest))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &resp)
	})
	return resp, found, err
}

// Put records resp under digest. Callers must never call this for a failed or
// partial response, for the same reason the campaign cache does not: a
// recorded error would deny every later campaign the retry that might have
// succeeded, and here it would do so forever.
func (r *AnswerRecord) Put(digest string, resp jev.Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return r.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(answerBucket).Put([]byte(digest), data) })
}
