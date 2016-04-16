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
package boltrec

import (
	"os"
	"testing"
	"time"

	"github.com/gravitational/teleport/lib/recorder"
	"github.com/gravitational/teleport/lib/recorder/test"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/utils"

	"gopkg.in/check.v1"
)

func TestBolt(t *testing.T) { check.TestingT(t) }

type BoltRecSuite struct {
	r       *boltRecorder
	suite   test.RecorderSuite
	dirPath string
	chunks  []recorder.Chunk
}

var _ = check.Suite(&BoltRecSuite{})

func (s *BoltRecSuite) SetUpSuite(c *check.C) {
	utils.InitLoggerForTests()
	//utils.InitLoggerDebug()
	s.dirPath = c.MkDir()
	s.chunks = []recorder.Chunk{
		{
			Data:           []byte("one"),
			Delay:          time.Second * 5,
			ServerID:       "server-one",
			TerminalParams: session.TerminalParams{W: 1, H: 2},
		},
		{
			Data:           []byte("two"),
			Delay:          time.Second * 10,
			ServerID:       "server-one",
			TerminalParams: session.TerminalParams{W: 1, H: 2},
		},
	}
}

func (s *BoltRecSuite) TearDownSuite(c *check.C) {
	os.RemoveAll(s.dirPath)
}

func (s *BoltRecSuite) TestFailureCreation(c *check.C) {
	const badPath = "/"
	recorder, err := New(badPath)

	c.Assert(err, check.NotNil)
	c.Assert(recorder, check.IsNil)
}

func (s *BoltRecSuite) TestSimpleSuccessfulCreation(c *check.C) {
	recorder, err := New(s.dirPath)
	c.Assert(err, check.IsNil)
	c.Assert(recorder, check.NotNil)
	c.Assert(recorder.Close(), check.IsNil)
}

func (s *BoltRecSuite) TestForceClose(c *check.C) {
	// create a new recorder and write some chunks
	recorder, _ := New(s.dirPath)
	w, _ := recorder.GetChunkWriter("force")
	w.WriteChunks(s.chunks)

	// close recorder without closing the writer
	c.Assert(recorder.Close(), check.IsNil)
	c.Assert(recorder.activeSessions(), check.Equals, 0)

	// now it should be safe to call .Close() on the orphan writer
	c.Assert(w.Close(), check.IsNil)
}

func (s *BoltRecSuite) TestReadWriteClose(c *check.C) {
	recorder, _ := New(s.dirPath)
	w, err := recorder.GetChunkWriter("one")
	c.Assert(err, check.IsNil)
	c.Assert(w, check.NotNil)
	r, err := recorder.GetChunkReader("one")
	c.Assert(err, check.IsNil)
	c.Assert(r, check.NotNil)

	c.Assert(w.WriteChunks(s.chunks), check.IsNil)
	chunks, err := r.ReadChunks(1, 3)
	c.Assert(err, check.IsNil)
	c.Assert(chunks, check.HasLen, len(s.chunks))
	c.Assert(chunks[0].Data, check.DeepEquals, s.chunks[0].Data)

	c.Assert(recorder.activeSessions(), check.Equals, 1)

	// closing writer should not destroy the session, because the reader
	// is still active
	c.Assert(w.Close(), check.IsNil)
	c.Assert(recorder.activeSessions(), check.Equals, 1)

	// close the reader and the session should disappear
	c.Assert(r.Close(), check.IsNil)
	c.Assert(recorder.activeSessions(), check.Equals, 0)

	// closing a recorder without active sessions should be safe
	c.Assert(recorder.Close(), check.IsNil)
	c.Assert(recorder.activeSessions(), check.Equals, 0)
}
