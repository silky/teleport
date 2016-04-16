/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package boltrec implements boltdb-backed session recorder
package boltrec

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/gravitational/teleport/lib/backend/boltbk"
	"github.com/gravitational/teleport/lib/recorder"

	log "github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/gravitational/trace"
)

// clientType enum defines which DB client is being requested: reader or a writer
type clientType int

const (
	typeReader = iota // 0
	typeWriter        // 1
	typeBoth          // 2
)

var (
	streamBufSize = 5
	// iterBucket is the name of the bucket in BoltDB where iterator
	// position is stored
	iterBucket = []string{"iter"}
	// iterKey is the key name inside 'iter' bucket where iteration
	// position is stored
	iterKey = []byte("val")
)

// New creates a new session recorder based on BoltDB. dirPath is the target
// directory where BoltDB files will be created, one per session.
func New(dirPath string) (*boltRecorder, error) {
	dirPath, err := filepath.Abs(dirPath)
	if err != nil {
		return nil, trace.Wrap(err, "failed to convert path")
	}
	if err := os.MkdirAll(dirPath, 0777); err != nil {
		return nil, trace.Wrap(
			err, fmt.Sprintf("failed to create '%v' for session records", dirPath))
	}
	// try creating a file in there to see if we have access:
	f, err := ioutil.TempFile(dirPath, "teleportsession")
	if err != nil {
		log.Error(err)
		return nil, trace.Wrap(err)
	}
	f.Close()
	defer os.Remove(f.Name())
	return &boltRecorder{
		path:     dirPath,
		sessions: make(map[string]*boltSession),
	}, nil
}

// boltRecorder implements recorder.Recorder interface using a single BoltDB database
// for every session.
type boltRecorder struct {
	sync.Mutex

	// path is the directory where all BoltDB database files reside
	path string

	// session is a map of sessionID -> boltSession
	sessions map[string]*boltSession
}

// boltSession represents an opened BoltDB database. It must be closed when
// a list of active clients (callers who requested Readers and Writers) becomes
// empty
type boltSession struct {
	sync.Mutex
	id       string
	db       *bolt.DB
	recorder *boltRecorder

	// list of callers who requested Writers and Readers. when this lsit
	// becomes empty, the session must be closed (and BoltDB too)
	activeClients []*boltSessionClient
}

// addClient adds a new reader or writer to the list of connected clients
// of this session.
func (s *boltSession) addClient(ct clientType) *boltSessionClient {
	c := &boltSessionClient{session: s, clientType: ct}
	s.Lock()
	defer s.Unlock()
	s.activeClients = append(s.activeClients, c)
	return c
}

// activeSessions returns a number of opened BoltDB files
func (r *boltRecorder) activeSessions() int {
	r.Lock()
	defer r.Unlock()
	return len(r.sessions)
}

// removeClient is called when a previously issued Reader or Writer gets
// closed
func (s *boltSession) removeClient(c *boltSessionClient) error {
	s.Lock()
	defer s.Unlock()

	for i, el := range s.activeClients {
		if el == c {
			s.activeClients = append(s.activeClients[:i], s.activeClients[i+1:]...)
			break
		}
	}

	// no more clients? we need to close this DB
	if len(s.activeClients) == 0 {
		s.recorder.removeSession(s.id)
	}
	return nil
}

// boltSessionClient represents a connected reader or writer to a BotlDB
type boltSessionClient struct {
	// implemnts this interface. When .Close() is called, it removes itself
	// from the list of connected clients on the parent session
	recorder.ChunkReadWriteCloser

	// parent session
	session *boltSession

	// clientType indicates if its' a reader or a writer or both
	clientType clientType
}

// removeSession removes a session from the recorder
func (r *boltRecorder) removeSession(sid string) {
	r.Lock()
	defer r.Unlock()
	s, found := r.sessions[sid]
	if s != nil && found {
		// close BoltDB:
		err := s.db.Close()
		if err != nil {
			log.Error(err)
		}
	}
	delete(r.sessions, sid)
	log.Infof("bolt recorder: session '%v' garbage-collected", sid)
}

// sessionFor returns a "DB session" for a given session ID. It will
// return an existing one, or create a new one
func (r *boltRecorder) sessionFor(sid string) (*boltSession, error) {
	r.Lock()
	defer r.Unlock()
	session, found := r.sessions[sid]
	if !found {
		db, err := r.openDatabase(sid)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		session = &boltSession{
			activeClients: make([]*boltSessionClient, 0),
			db:            db,
			id:            sid,
			recorder:      r,
		}
		r.sessions[sid] = session
	}
	return session, nil
}

// openDatabase opens or creates a new BoltDB database for a given session ID
func (r *boltRecorder) openDatabase(sid string) (*bolt.DB, error) {
	dbFilePath := filepath.Join(r.path, sid)
	log.Infof("boltrecorder.openDatabase(%v)", dbFilePath)

	// open database
	db, err := bolt.Open(dbFilePath, 0600, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// cleanup helper to be called on critical errors below
	cleanupIf := func(err error) error {
		if err != nil {
			db.Close()
			os.Remove(dbFilePath)
			log.Error(err)
		}
		return err
	}
	// read the current iterator value:
	err = db.View(func(tx *bolt.Tx) error {
		bkt, err := boltbk.GetBucket(tx, iterBucket)
		if err != nil {
			return err
		}
		bytes := bkt.Get(iterKey)
		if bytes == nil {
			return trace.NotFound("not found")
		}
		return nil
	})
	if err != nil {
		// some other error: clean up and return
		if !trace.IsNotFound(err) {
			return nil, trace.Wrap(cleanupIf(err))
		}
		// drop '0' as the current iterator value:
		err = db.Update(func(tx *bolt.Tx) error {
			bkt, err := boltbk.UpsertBucket(tx, iterBucket)
			if err != nil {
				return err
			}
			var bin = make([]byte, 8)
			binary.BigEndian.PutUint64(bin, 0)
			return bkt.Put(iterKey, bin)
		})
		if err != nil {
			return nil, trace.Wrap(cleanupIf(err))
		}
	}
	return db, nil
}

// Close closes all BoltDB instances
func (r *boltRecorder) Close() error {
	cloneSessions := func() []*boltSession {
		r.Lock()
		defer r.Unlock()
		localCopy := make([]*boltSession, len(r.sessions))
		i := 0
		for _, s := range r.sessions {
			localCopy[i] = s
			i++
		}
		r.sessions = make(map[string]*boltSession)
		return localCopy
	}
	// copy all active sessions into a local slice (to minimize total lock time),
	// then close them all
	for _, s := range cloneSessions() {
		for _, client := range s.activeClients {
			err := client.Close()
			if err != nil {
				log.Error(err)
			}
		}
	}
	return nil
}

// GetChunkWriter creates and returns a new chunk writer object
func (r *boltRecorder) GetChunkWriter(sid string) (recorder.ChunkWriteCloser, error) {
	session, err := r.sessionFor(sid)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	log.Infof("boltRecorder.getChunkWriter(%v) -> %p", sid, session)
	return session.addClient(typeWriter), nil
}

// GetChunkWriter creates and returns a new chunk reader object
func (r *boltRecorder) GetChunkReader(sid string) (recorder.ChunkReadCloser, error) {
	session, err := r.sessionFor(sid)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	log.Infof("boltRecorder.getChunkReader(%v) -> %p", sid, session)
	return session.addClient(typeReader), nil
}

func (c *boltSessionClient) GetChunksCount() (uint64, error) {
	db := c.session.db
	var lastChunk uint64
	err := db.View(func(tx *bolt.Tx) error {
		bkt, err := boltbk.GetBucket(tx, iterBucket)
		if err != nil {
			return err
		}
		bytes := bkt.Get(iterKey)
		if bytes == nil {
			return trace.NotFound("not found")
		}
		lastChunk = binary.BigEndian.Uint64(bytes)
		return nil
	})
	return lastChunk, trace.Wrap(err)
}

func (c *boltSessionClient) WriteChunks(ch []recorder.Chunk) error {
	db := c.session.db
	return db.Update(func(tx *bolt.Tx) error {
		ibkt, err := boltbk.GetBucket(tx, iterBucket)
		if err != nil {
			return err
		}
		iterb := ibkt.Get(iterKey)
		if iterb == nil {
			return trace.NotFound("iter not found")
		}
		lastChunk := binary.BigEndian.Uint64(iterb)
		cbkt, err := boltbk.UpsertBucket(tx, []string{"chunks"})
		if err != nil {
			return err
		}
		bin := make([]byte, 8)
		for _, c := range ch {
			chunkb, err := json.Marshal(c)
			if err != nil {
				return err
			}
			lastChunk++
			binary.BigEndian.PutUint64(bin, lastChunk)
			if err := cbkt.Put(bin, chunkb); err != nil {
				return err
			}
		}
		return ibkt.Put(iterKey, bin)
	})
}

func (c *boltSessionClient) ReadChunk(chunk uint64) ([]byte, error) {
	db := c.session.db
	var bt []byte
	err := db.View(func(tx *bolt.Tx) error {
		bin := make([]byte, 8)
		binary.BigEndian.PutUint64(bin, chunk)
		cbkt, err := boltbk.GetBucket(tx, []string{"chunks"})
		if err != nil {
			return err
		}
		bytes := cbkt.Get(bin)
		if bytes == nil {
			return trace.NotFound("chunk not found")
		}
		bt = make([]byte, len(bytes))
		copy(bt, bytes)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bt, nil
}

func (c *boltSessionClient) Close() error {
	return c.session.removeClient(c)
}

func (c *boltSessionClient) ReadChunks(start int, end int) ([]recorder.Chunk, error) {
	chunks := []recorder.Chunk{}
	for i := start; i < end; i++ {
		out, err := c.ReadChunk(uint64(i))
		if err != nil {
			if trace.IsNotFound(err) {
				return chunks, nil
			}
			return nil, err
		}
		var ch *recorder.Chunk
		if err := json.Unmarshal(out, &ch); err != nil {
			return nil, err
		}
		chunks = append(chunks, *ch)
	}
	return chunks, nil
}

func (c *boltSessionClient) GetStreamReader() io.Reader {
	return nil
}
