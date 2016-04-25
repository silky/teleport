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

package web

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	authority "github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/boltbk"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/services/suite"
	"github.com/gravitational/teleport/lib/session"
	sess "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gokyle/hotp"
	"github.com/gravitational/roundtrip"
	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	. "gopkg.in/check.v1"
)

func TestWeb(t *testing.T) {
	utils.InitLoggerForTests()
	TestingT(t)
}

type WebSuite struct {
	node        *srv.Server
	srvAddress  string
	srvID       string
	srvHostPort string
	bk          backend.Backend
	roleAuth    *auth.AuthWithRoles
	dir         string
	user        string
	domainName  string
	signer      ssh.Signer
	tunServer   *auth.AuthTunnel
	webServer   *httptest.Server
	freePorts   []string
}

var _ = Suite(&WebSuite{})

func (s *WebSuite) SetUpTest(c *C) {
	s.dir = c.MkDir()

	u, err := user.Current()
	c.Assert(err, IsNil)
	s.user = u.Username

	s.freePorts, err = utils.GetFreeTCPPorts(3)
	c.Assert(err, IsNil)

	s.bk, err = boltbk.New(filepath.Join(s.dir, "db"))
	c.Assert(err, IsNil)

	s.domainName = "localhost"
	authServer := auth.NewAuthServer(&auth.InitConfig{
		Backend:    s.bk,
		Authority:  authority.New(),
		DomainName: s.domainName})

	c.Assert(authServer.UpsertCertAuthority(
		*suite.NewTestCA(services.UserCA, s.domainName), backend.Forever), IsNil)
	c.Assert(authServer.UpsertCertAuthority(
		*suite.NewTestCA(services.HostCA, s.domainName), backend.Forever), IsNil)

	sessionServer, err := sess.New(s.bk)
	c.Assert(err, IsNil)

	alog, err := events.NewAuditLog(s.dir, true)
	c.Assert(err, IsNil)

	s.roleAuth = auth.NewAuthWithRoles(authServer,
		auth.NewStandardPermissions(),
		sessionServer,
		teleport.RoleAdmin,
		alog)

	// set up host private key and certificate
	hpriv, hpub, err := authServer.GenerateKeyPair("")
	c.Assert(err, IsNil)
	hcert, err := authServer.GenerateHostCert(
		hpub, s.domainName, s.domainName, teleport.RoleAdmin, 0)
	c.Assert(err, IsNil)

	// set up user CA and set up a user that has access to the server
	s.signer, err = sshutils.NewSigner(hpriv, hcert)
	c.Assert(err, IsNil)

	// start node
	nodePort := s.freePorts[len(s.freePorts)-1]
	s.freePorts = s.freePorts[:len(s.freePorts)-1]

	s.srvAddress = fmt.Sprintf("127.0.0.1:%v", nodePort)
	node, err := srv.New(
		utils.NetAddr{AddrNetwork: "tcp", Addr: s.srvAddress},
		s.domainName,
		[]ssh.Signer{s.signer},
		s.roleAuth,
		s.dir,
		nil,
		srv.SetShell("/bin/sh"),
		srv.SetSessionServer(sessionServer),
	)
	c.Assert(err, IsNil)
	s.node = node
	s.srvID = node.ID()

	c.Assert(s.node.Start(), IsNil)

	revTunServer, err := reversetunnel.NewServer(
		utils.NetAddr{
			AddrNetwork: "tcp",
			Addr:        fmt.Sprintf("%v:0", s.domainName),
		},
		[]ssh.Signer{s.signer},
		s.roleAuth,
		reversetunnel.ServerTimeout(200*time.Millisecond),
		reversetunnel.DirectSite(s.domainName, s.roleAuth),
	)
	c.Assert(err, IsNil)

	apiPort := s.freePorts[len(s.freePorts)-1]
	s.freePorts = s.freePorts[:len(s.freePorts)-1]

	apiServer := auth.NewAPIWithRoles(auth.APIConfig{
		AuthServer:        authServer,
		AuditLog:          alog,
		SessionService:    sessionServer,
		PermissionChecker: auth.NewAllowAllPermissions(),
		Roles:             auth.StandardRoles})
	go apiServer.Serve()

	tunAddr := utils.NetAddr{
		AddrNetwork: "tcp", Addr: fmt.Sprintf("127.0.0.1:%v", apiPort),
	}

	s.tunServer, err = auth.NewTunnel(
		tunAddr,
		[]ssh.Signer{s.signer},
		apiServer, authServer)
	c.Assert(err, IsNil)
	c.Assert(s.tunServer.Start(), IsNil)

	// start handler
	assetsDir, err := filepath.Abs("../../web/dist")
	c.Assert(err, IsNil)
	handler, err := NewHandler(Config{
		Proxy:       revTunServer,
		AssetsDir:   assetsDir,
		AuthServers: tunAddr,
		DomainName:  s.domainName,
	}, SetSessionStreamPollPeriod(200*time.Millisecond))
	c.Assert(err, IsNil)

	s.webServer = httptest.NewUnstartedServer(handler)
	s.webServer.StartTLS()
}

func (s *WebSuite) url() *url.URL {
	u, err := url.Parse("https://" + s.webServer.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	return u
}

func (s *WebSuite) client(opts ...roundtrip.ClientParam) *webClient {
	opts = append(opts, roundtrip.HTTPClient(newInsecureClient()))
	clt, err := newWebClient(s.url().String(), opts...)
	if err != nil {
		panic(err)
	}
	return clt
}

func (s *WebSuite) TearDownTest(c *C) {
	c.Assert(s.node.Close(), IsNil)
	c.Assert(s.tunServer.Close(), IsNil)
	s.webServer.Close()
}

func (s *WebSuite) TestNewUser(c *C) {
	token, err := s.roleAuth.CreateSignupToken(&services.TeleportUser{Name: "bob", AllowedLogins: []string{s.user}})
	c.Assert(err, IsNil)

	clt := s.client()
	re, err := clt.Get(clt.Endpoint("webapi", "users", "invites", token), url.Values{})
	c.Assert(err, IsNil)

	var out *renderUserInviteResponse
	c.Assert(json.Unmarshal(re.Bytes(), &out), IsNil)
	c.Assert(out.User, Equals, "bob")
	c.Assert(out.InviteToken, Equals, token)

	_, _, hotpValues, err := s.roleAuth.GetSignupTokenData(token)
	c.Assert(err, IsNil)

	tempPass := "abc123"

	re, err = clt.PostJSON(clt.Endpoint("webapi", "users"), createNewUserReq{
		InviteToken:       token,
		Pass:              tempPass,
		SecondFactorToken: hotpValues[0],
	})
	c.Assert(err, IsNil)

	var rawSess *createSessionResponseRaw
	c.Assert(json.Unmarshal(re.Bytes(), &rawSess), IsNil)
	cookies := re.Cookies()
	c.Assert(len(cookies), Equals, 1)

	// now make sure we are logged in by calling authenticated method
	// we need to supply both session cookie and bearer token for
	// request to succeed
	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt = s.client(roundtrip.BearerAuth(rawSess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	re, err = clt.Get(clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// in absense of session cookie or bearer auth the same request fill fail

	// no session cookie:
	clt = s.client(roundtrip.BearerAuth(rawSess.Token))
	re, err = clt.Get(clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)

	// no bearer token:
	clt = s.client(roundtrip.CookieJar(jar))
	re, err = clt.Get(clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

type authPack struct {
	user    string
	otp     *hotp.HOTP
	session *CreateSessionResponse
	clt     *webClient
	cookies []*http.Cookie
}

func (s *WebSuite) authPackFromResponse(c *C, re *roundtrip.Response) *authPack {
	var sess *createSessionResponseRaw
	c.Assert(json.Unmarshal(re.Bytes(), &sess), IsNil)

	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt := s.client(roundtrip.BearerAuth(sess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	session, err := sess.response()
	if err != nil {
		panic(err)
	}
	return &authPack{
		user:    session.User.GetName(),
		session: session,
		clt:     clt,
		cookies: re.Cookies(),
	}
}

// authPack returns new authenticated package consisting
// of created valid user, hotp token, created web session and
// authenticated client
func (s *WebSuite) authPack(c *C) *authPack {
	user := s.user
	pass := "abc123"

	hotpURL, _, err := s.roleAuth.UpsertPassword(user, []byte(pass))
	c.Assert(err, IsNil)
	otp, _, err := hotp.FromURL(hotpURL)
	c.Assert(err, IsNil)
	otp.Increment()

	err = s.roleAuth.UpsertUser(
		&services.TeleportUser{Name: user, AllowedLogins: []string{s.user}})
	c.Assert(err, IsNil)

	clt := s.client()

	re, err := clt.PostJSON(clt.Endpoint("webapi", "sessions"), createSessionReq{
		User:              user,
		Pass:              pass,
		SecondFactorToken: otp.OTP(),
	})
	c.Assert(err, IsNil)

	var rawSess *createSessionResponseRaw
	c.Assert(json.Unmarshal(re.Bytes(), &rawSess), IsNil)

	sess, err := rawSess.response()
	c.Assert(err, IsNil)

	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt = s.client(roundtrip.BearerAuth(sess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	return &authPack{
		user:    user,
		session: sess,
		clt:     clt,
		cookies: re.Cookies(),
	}
}

func (s *WebSuite) TestWebSessionsCRUD(c *C) {
	pack := s.authPack(c)

	// make sure we can use client to make authenticated requests
	re, err := pack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// now delete session
	_, err = pack.clt.Delete(
		pack.clt.Endpoint("webapi", "sessions", pack.session.Token))
	c.Assert(err, IsNil)

	// subsequent requests trying to use this session will fail
	re, err = pack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestWebSessionsLogout(c *C) {
	pack := s.authPack(c)

	// make sure we can use client to make authenticated requests
	re, err := pack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// now delete session
	var redirectErr = trace.Errorf("attempted redirect")
	pack.clt.HTTPClient().CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return redirectErr
	}
	re, err = pack.clt.Get(pack.clt.Endpoint("webapi", "logout"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Matches, ".*attempted redirect.*")

	// subsequent requests trying to use this session will fail
	re, err = pack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestWebSessionsRenew(c *C) {
	pack := s.authPack(c)

	// make sure we can use client to make authenticated requests
	// before we issue this request, we will recover session id and bearer token
	//
	prevSessionCookie := *pack.cookies[0]
	prevBearerToken := pack.session.Token
	re, err := pack.clt.PostJSON(pack.clt.Endpoint("webapi", "sessions", "renew"), nil)
	c.Assert(err, IsNil)

	newPack := s.authPackFromResponse(c, re)

	// new session is functioning
	re, err = newPack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	// old session is stil valid too (until it expires)
	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)
	oldClt := s.client(roundtrip.BearerAuth(prevBearerToken), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), []*http.Cookie{&prevSessionCookie})
	re, err = oldClt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	// now delete session
	_, err = newPack.clt.Delete(
		pack.clt.Endpoint("webapi", "sessions", newPack.session.Token))
	c.Assert(err, IsNil)

	// subsequent requests trying to use this session will fail
	re, err = newPack.clt.Get(pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestWebSessionsBadInput(c *C) {
	user := "bob"
	pass := "abc123"

	hotpURL, _, err := s.roleAuth.UpsertPassword(user, []byte(pass))
	c.Assert(err, IsNil)
	otp, _, err := hotp.FromURL(hotpURL)
	c.Assert(err, IsNil)
	otp.Increment()

	clt := s.client()

	token := otp.OTP()

	reqs := []createSessionReq{
		// emtpy request
		{},
		// missing user
		{
			Pass:              pass,
			SecondFactorToken: token,
		},
		// missing pass
		{
			User:              user,
			SecondFactorToken: token,
		},
		// bad pass
		{
			User:              user,
			Pass:              "bla bla",
			SecondFactorToken: token,
		},
		// bad hotp token
		{
			User:              user,
			Pass:              pass,
			SecondFactorToken: "bad token",
		},
		// missing hotp token
		{
			User: user,
			Pass: pass,
		},
	}
	for i, req := range reqs {
		_, err = clt.PostJSON(clt.Endpoint("webapi", "sessions"), req)
		c.Assert(err, NotNil, Commentf("tc %v", i))
		c.Assert(trace.IsAccessDenied(err), Equals, true, Commentf("tc %v %T is not access denied", i, err))
	}
}

func (s *WebSuite) TestGetSiteNodes(c *C) {
	pack := s.authPack(c)

	// get site nodes
	re, err := pack.clt.Get(pack.clt.Endpoint("webapi", "sites", s.domainName, "nodes"), url.Values{})
	c.Assert(err, IsNil)

	var nodes *getSiteNodesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &nodes), IsNil)
	c.Assert(len(nodes.Nodes), Equals, 1)

	// get site nodes using shortcut
	re, err = pack.clt.Get(pack.clt.Endpoint("webapi", "sites", currentSiteShortcut, "nodes"), url.Values{})
	c.Assert(err, IsNil)

	var nodes2 *getSiteNodesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &nodes2), IsNil)
	c.Assert(len(nodes.Nodes), Equals, 1)

	c.Assert(nodes2, DeepEquals, nodes)
}

func (s *WebSuite) connect(c *C, pack *authPack, opts ...session.ID) *websocket.Conn {
	var sessionID session.ID
	if len(opts) == 0 {
		sessionID = session.NewID()
	} else {
		sessionID = opts[0]
	}
	u := url.URL{Host: s.url().Host, Scheme: WSS, Path: fmt.Sprintf("/v1/webapi/sites/%v/connect", currentSiteShortcut)}
	data, err := json.Marshal(connectReq{
		ServerID:  s.srvID,
		Login:     s.user,
		Term:      session.TerminalParams{W: 100, H: 100},
		SessionID: sessionID,
	})
	c.Assert(err, IsNil)

	q := u.Query()
	q.Set("params", string(data))
	q.Set(roundtrip.AccessTokenQueryParam, pack.session.Token)
	u.RawQuery = q.Encode()

	wscfg, err := websocket.NewConfig(u.String(), "http://localhost")
	wscfg.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	c.Assert(err, IsNil)
	for _, cookie := range pack.cookies {
		wscfg.Header.Add("Cookie", cookie.String())
	}
	clt, err := websocket.DialConfig(wscfg)
	c.Assert(err, IsNil)

	return clt
}

func (s *WebSuite) sessionStream(c *C, pack *authPack, sessionID session.ID, opts ...string) *websocket.Conn {
	u := url.URL{
		Host:   s.url().Host,
		Scheme: WSS,
		Path: fmt.Sprintf(
			"/v1/webapi/sites/%v/sessions/%v/events/stream",
			currentSiteShortcut,
			sessionID),
	}
	q := u.Query()
	q.Set(roundtrip.AccessTokenQueryParam, pack.session.Token)
	u.RawQuery = q.Encode()
	wscfg, err := websocket.NewConfig(u.String(), "http://localhost")
	wscfg.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	c.Assert(err, IsNil)
	for _, cookie := range pack.cookies {
		wscfg.Header.Add("Cookie", cookie.String())
	}
	clt, err := websocket.DialConfig(wscfg)
	c.Assert(err, IsNil)

	return clt
}

func (s *WebSuite) TestConnect(c *C) {
	clt := s.connect(c, s.authPack(c))
	defer clt.Close()

	_, err := io.WriteString(clt, "expr 137 + 39\r\n")
	c.Assert(err, IsNil)

	resultC := make(chan struct{})

	go func() {
		out := make([]byte, 100)
		for {
			n, err := clt.Read(out)
			c.Assert(err, IsNil)
			decoded, err := base64.StdEncoding.DecodeString(string(out[:n]))
			c.Assert(err, IsNil)
			if strings.Contains(removeSpace(string(decoded)), "176") {
				close(resultC)
				return
			}
		}
	}()

	select {
	case <-time.After(time.Second):
		c.Fatalf("timeout waiting for proper response")
	case <-resultC:
		// everything is as expected
	}

}

func (s *WebSuite) TestNodesWithSessions(c *C) {
	sid := session.NewID()
	pack := s.authPack(c)
	clt := s.connect(c, pack, sid)
	defer clt.Close()

	// to make sure we have a session
	_, err := io.WriteString(clt, "expr 137 + 39\r\n")
	c.Assert(err, IsNil)

	// make sure server has replied
	out := make([]byte, 100)
	clt.Read(out)

	var nodes *getSiteNodesResponse
	for i := 0; i < 3; i++ {
		// get site nodes and make sure the node has our active party
		re, err := pack.clt.Get(pack.clt.Endpoint("webapi", "sites", s.domainName, "nodes"), url.Values{})
		c.Assert(err, IsNil)

		c.Assert(json.Unmarshal(re.Bytes(), &nodes), IsNil)
		c.Assert(len(nodes.Nodes), Equals, 1)

		if len(nodes.Nodes[0].Sessions) == 1 {
			break
		}
		// sessions do not appear momentarily as there's async heartbeat
		// procedure
		time.Sleep(20 * time.Millisecond)
	}

	c.Assert(len(nodes.Nodes[0].Sessions), Equals, 1)
	c.Assert(nodes.Nodes[0].Sessions[0].ID, Equals, sid)

	// connect to session stream and receive events
	stream := s.sessionStream(c, pack, sid)
	defer stream.Close()
	var event *sessionStreamEvent
	c.Assert(websocket.JSON.Receive(stream, &event), IsNil)
	c.Assert(event, NotNil)
	// TODO (ev)
	//c.Assert(getEvent(events.SessionEvent, event.Events), NotNil)

	// one more party joins the session
	clt2 := s.connect(c, pack, sid)
	defer clt2.Close()

	// to make sure we have a session
	_, err = io.WriteString(clt, "expr 147 + 29\r\n")
	c.Assert(err, IsNil)

	c.Assert(websocket.JSON.Receive(stream, &event), IsNil)
	c.Assert(len(event.Session.Parties), Equals, 2)
}

func (s *WebSuite) TestCloseConnectionsOnLogout(c *C) {
	sid := session.NewID()
	pack := s.authPack(c)
	clt := s.connect(c, pack, sid)
	defer clt.Close()

	// to make sure we have a session
	_, err := io.WriteString(clt, "expr 137 + 39\r\n")
	c.Assert(err, IsNil)

	// make sure server has replied
	out := make([]byte, 100)
	clt.Read(out)

	_, err = pack.clt.Delete(
		pack.clt.Endpoint("webapi", "sessions", pack.session.Token))
	c.Assert(err, IsNil)

	// wait until we timeout or detect that connection has been closed
	after := time.After(time.Second)
	errC := make(chan error)
	go func() {
		for {
			_, err := clt.Read(out)
			if err != nil {
				errC <- err
			}
		}
	}()

	select {
	case <-after:
		c.Fatalf("timeout")
	case err := <-errC:
		c.Assert(err, Equals, io.EOF)
	}
}

func (s *WebSuite) TestCreateSession(c *C) {
	pack := s.authPack(c)

	sess := session.Session{
		TerminalParams: session.TerminalParams{W: 300, H: 120},
		Login:          s.user,
	}

	re, err := pack.clt.PostJSON(
		pack.clt.Endpoint("webapi", "sites", s.domainName, "sessions"),
		siteSessionGenerateReq{Session: sess},
	)
	c.Assert(err, IsNil)

	var created *siteSessionGenerateResponse
	c.Assert(json.Unmarshal(re.Bytes(), &created), IsNil)
	c.Assert(created.Session.ID, Not(Equals), "")
}

func (s *WebSuite) TestResizeTerminal(c *C) {
	sid := session.NewID()
	pack := s.authPack(c)
	clt := s.connect(c, pack, sid)
	defer clt.Close()

	// to make sure we have a session
	_, err := io.WriteString(clt, "expr 137 + 39\r\n")
	c.Assert(err, IsNil)

	// make sure server has replied
	out := make([]byte, 100)
	clt.Read(out)

	params := session.TerminalParams{W: 300, H: 120}
	_, err = pack.clt.PutJSON(
		pack.clt.Endpoint("webapi", "sites", s.domainName, "sessions", string(sid)),
		siteSessionUpdateReq{TerminalParams: session.TerminalParams{W: 300, H: 120}},
	)
	c.Assert(err, IsNil)

	re, err := pack.clt.Get(
		pack.clt.Endpoint("webapi", "sites", s.domainName, "sessions", string(sid)), url.Values{})
	c.Assert(err, IsNil)

	var sess *siteSessionGetResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sess), IsNil)
	c.Assert(sess.Session.TerminalParams, DeepEquals, params)
}

func (s *WebSuite) TestPlayback(c *C) {
	sid := session.NewID()
	pack := s.authPack(c)
	clt := s.connect(c, pack, sid)
	defer clt.Close()

	// TODO (ev) implement this
}

func (s *WebSuite) TestSessionEvents(c *C) {
	sid := session.NewID()
	pack := s.authPack(c)
	clt := s.connect(c, pack, sid)
	defer clt.Close()

	// TODO (ev) implement this
}

func removeSpace(in string) string {
	for _, c := range []string{"\n", "\r", "\t"} {
		in = strings.Replace(in, c, " ", -1)
	}
	return strings.TrimSpace(in)
}
