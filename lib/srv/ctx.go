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

package srv

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	rsession "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/codahale/lunk"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var ctxID int32

// subsystemResult is a result of execution of the subsystem
type subsystemResult struct {
	err error
}

// ctx holds session specific context, such as SSH auth agents
// PTYs, and other resources. ctx can be used to attach resources
// that should be closed once the session closes.
type ctx struct {
	*log.Entry
	// env is a list of environment variables passed to the session
	env map[string]string

	// srv is a pointer to the server holding the context
	srv *Server

	// this event id will be associated with all events emitted with this context
	eid lunk.EventID

	// server specific incremental session id
	id int
	// info about connection for debugging purposes
	info ssh.ConnMetadata

	sync.RWMutex
	// term holds PTY if it was requested by the session
	term *terminal

	// agent is a client to remote SSH agent
	agent agent.Agent
	// agentCh is SSH channel using SSH agent protocol
	agentCh ssh.Channel

	// result channel will be used by remote executions
	// that are processed in separate process, once the result is collected
	// they would send the result to this channel
	result chan execResult

	// close used by channel operations asking to close the session
	subsystemResultC chan subsystemResult

	// closers is a list of io.Closer that will be called when session closes
	// this is handy as sometimes client closes session, in this case resources
	// will be properly closed and deallocated, otherwise they could be kept hanging
	closers []io.Closer

	// teleportUser is a teleport user that was used to log in
	teleportUser string

	// login is operating system user login chosen by the user
	login string

	// isTestStub is set to True by tests
	isTestStub bool

	// session, if there's an active one
	session *session
}

// emit emits event
func (c *ctx) emit(e lunk.Event) {
	c.srv.elog.Log(c.eid, e)
}

// addCloser adds any closer in ctx that will be called
// whenever server closes session channel
func (c *ctx) addCloser(closer io.Closer) {
	c.Lock()
	defer c.Unlock()
	c.closers = append(c.closers, closer)
}

func (c *ctx) getAgent() agent.Agent {
	c.RLock()
	defer c.RUnlock()
	return c.agent
}

func (c *ctx) setAgent(a agent.Agent, ch ssh.Channel) {
	c.Lock()
	defer c.Unlock()
	if c.agentCh != nil {
		c.Infof("closing previous agent channel")
		c.agentCh.Close()
	}
	c.agentCh = ch
	c.agent = a
}

func (c *ctx) getTerm() *terminal {
	c.RLock()
	defer c.RUnlock()
	return c.term
}

func (c *ctx) setTerm(t *terminal) {
	c.Lock()
	defer c.Unlock()
	log.Infof("setTerm: %v", t)
	c.term = t
}

// takeClosers returns all resources that should be closed and sets the properties to null
// we do this to avoid calling Close() under lock to avoid potential deadlocks
func (c *ctx) takeClosers() []io.Closer {
	// this is done to avoid any operation holding the lock for too long
	c.Lock()
	defer c.Unlock()
	closers := []io.Closer{}
	if c.term != nil {
		closers = append(closers, c.term)
		c.term = nil
	}
	if c.agentCh != nil {
		closers = append(closers, c.agentCh)
		c.agentCh = nil
	}
	closers = append(closers, c.closers...)
	c.closers = nil
	return closers
}

func (c *ctx) Close() error {
	return closeAll(c.takeClosers()...)
}

func (c *ctx) sendResult(r execResult) {
	select {
	case c.result <- r:
	default:
		log.Infof("blocked on sending exec result %v", r)
	}
}

func (c *ctx) sendSubsystemResult(err error) {
	select {
	case c.subsystemResultC <- subsystemResult{err: err}:
	default:
		c.Infof("blocked on sending close request")
	}
}

func (c *ctx) resolver() resolver {
	return c.srv.resolver
}

func (c *ctx) String() string {
	return fmt.Sprintf("sess(%v->%v, user=%v, id=%v)", c.info.RemoteAddr(), c.info.LocalAddr(), c.info.User(), c.id)
}

func (c *ctx) setEnv(key, val string) {
	c.Infof("setEnv(%v=%v)", key, val)
	c.env[key] = val
}

func (c *ctx) getEnv(key string) (string, bool) {
	val, ok := c.env[key]
	return val, ok
}

func (c *ctx) getSessionID() (*rsession.ID, error) {
	sid, ok := c.getEnv(sshutils.SessionEnvVar)
	if !ok || sid == "" {
		return nil, trace.NotFound("session ID not found")
	}
	sessionID, err := rsession.ParseID(sid)
	if err != nil {
		return nil, trace.BadParameter("%v session ID: bad format", sshutils.SessionEnvVar)
	}
	return sessionID, nil
}

func (c *ctx) initSessionID() (*rsession.ID, error) {
	sessionID, err := c.getSessionID()
	if err == nil {
		return sessionID, nil
	}
	if trace.IsNotFound(err) {
		sid := rsession.NewID()
		c.setEnv(sshutils.SessionEnvVar, string(sid))
		return &sid, nil
	}
	return nil, trace.Wrap(err)
}

func newCtx(srv *Server, conn *ssh.ServerConn) *ctx {
	ctx := &ctx{
		env:              make(map[string]string),
		eid:              lunk.NewRootEventID(),
		info:             conn,
		id:               int(atomic.AddInt32(&ctxID, int32(1))),
		result:           make(chan execResult, 10),
		subsystemResultC: make(chan subsystemResult, 10),
		srv:              srv,
		teleportUser:     conn.Permissions.Extensions[utils.CertTeleportUser],
		login:            conn.User(),
	}
	ctx.Entry = log.WithFields(srv.logFields(log.Fields{
		"local":        conn.LocalAddr(),
		"remote":       conn.RemoteAddr(),
		"login":        ctx.login,
		"teleportUser": ctx.teleportUser,
		"id":           ctx.id,
	}))
	return ctx
}
