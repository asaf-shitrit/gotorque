package campaign

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	stateBucket = []byte("state")
	eventBucket = []byte("events")
	stateKey    = []byte("campaign")
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
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(stateBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(eventBucket)
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
