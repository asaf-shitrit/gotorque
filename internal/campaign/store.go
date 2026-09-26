package campaign

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"example.com/gotorque/internal/jev"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

var (
	stateBucket    = []byte("state")
	eventBucket    = []byte("events")
	jevCacheBucket = []byte("jev_cache")
	stateKey       = []byte("campaign")
)

type Store struct{ db *bolt.DB }

func OpenStore(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("campaign database path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		// bbolt reports a lock it could not acquire within Options.Timeout as a
		// bare "timeout", which reads like a filesystem or disk fault. The only
		// thing that holds this file's exclusive lock is another gotorque
		// process working the same campaign directory, so say that instead of
		// leaving an operator to guess.
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("another gotorque process holds the campaign database %s: %w", path, err)
		}
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(stateBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(eventBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(jevCacheBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Save(state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(stateBucket).Put(stateKey, data) })
}

func (s *Store) Load() (State, error) {
	var state State
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(stateBucket).Get(stateKey)
		if data == nil {
			return errors.New("campaign state not found")
		}
		return json.Unmarshal(data, &state)
	})
	return state, err
}

func (s *Store) Append(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(eventBucket)
		sequence, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		key := []byte(fmt.Sprintf("%020d", sequence))
		return bucket.Put(key, data)
	})
}

// JevCacheGet returns a previously cached Jev response for the given digest
// (see jev.Digest), keyed by request rather than by role: a state and
// question set the analyst and, say, a resumed campaign both build the same
// way answers identically either way. A miss is reported by ok == false with
// a nil error.
func (s *Store) JevCacheGet(digest string) (jev.Response, bool, error) {
	var resp jev.Response
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(jevCacheBucket).Get([]byte(digest))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &resp)
	})
	return resp, found, err
}

// JevCachePut persists resp under digest. Callers must never call this for a
// failed or partial response: a cached error would deny every later request
// the retry that might have succeeded.
func (s *Store) JevCachePut(digest string, resp jev.Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(jevCacheBucket).Put([]byte(digest), data) })
}

func (s *Store) Events() ([]Event, error) {
	var events []Event
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(eventBucket).ForEach(func(_, value []byte) error {
			var event Event
			if err := json.Unmarshal(value, &event); err != nil {
				return err
			}
			events = append(events, event)
			return nil
		})
	})
	return events, err
}
