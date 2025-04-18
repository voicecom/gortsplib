package gortsplib

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	gourl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/voicecom/gortsplib/v4/pkg/base"
	"github.com/voicecom/gortsplib/v4/pkg/bytecounter"
	"github.com/voicecom/gortsplib/v4/pkg/conn"
	"github.com/voicecom/gortsplib/v4/pkg/description"
	"github.com/voicecom/gortsplib/v4/pkg/liberrors"
)

func getSessionID(header base.Header) string {
	if h, ok := header["Session"]; ok && len(h) == 1 {
		return h[0]
	}
	return ""
}

func serverSideDescription(d *description.Session) *description.Session {
	out := &description.Session{
		Title:     d.Title,
		FECGroups: d.FECGroups,
		Medias:    make([]*description.Media, len(d.Medias)),
	}

	for i, medi := range d.Medias {
		out.Medias[i] = &description.Media{
			Type:          medi.Type,
			ID:            medi.ID,
			IsBackChannel: medi.IsBackChannel,
			// we have to use trackID=number in order to support clients
			// like the Grandstream GXV3500.
			Control: "trackID=" + strconv.FormatInt(int64(i), 10),
			Formats: medi.Formats,
		}
	}

	return out
}

type readReq struct {
	req *base.Request
	res chan error
}

// ServerConn is a server-side RTSP connection.
type ServerConn struct {
	s     *Server
	nconn net.Conn

	ctx        context.Context
	ctxCancel  func()
	userData   interface{}
	remoteAddr *net.TCPAddr
	bc         *bytecounter.ByteCounter
	conn       *conn.Conn
	session    *ServerSession

	// in
	chReadRequest   chan readReq
	chReadError     chan error
	chRemoveSession chan *ServerSession

	// out
	done chan struct{}
}

func (sc *ServerConn) initialize() {
	ctx, ctxCancel := context.WithCancel(sc.s.ctx)

	if sc.s.TLSConfig != nil {
		sc.nconn = tls.Server(sc.nconn, sc.s.TLSConfig)
	}

	sc.bc = bytecounter.New(sc.nconn, nil, nil)
	sc.ctx = ctx
	sc.ctxCancel = ctxCancel
	sc.remoteAddr = sc.nconn.RemoteAddr().(*net.TCPAddr)
	sc.chReadRequest = make(chan readReq)
	sc.chReadError = make(chan error)
	sc.chRemoveSession = make(chan *ServerSession)
	sc.done = make(chan struct{})

	sc.s.wg.Add(1)
	go sc.run()
}

// Close closes the ServerConn.
func (sc *ServerConn) Close() {
	sc.ctxCancel()
}

// NetConn returns the underlying net.Conn.
func (sc *ServerConn) NetConn() net.Conn {
	return sc.nconn
}

// BytesReceived returns the number of read bytes.
func (sc *ServerConn) BytesReceived() uint64 {
	return sc.bc.BytesReceived()
}

// BytesSent returns the number of written bytes.
func (sc *ServerConn) BytesSent() uint64 {
	return sc.bc.BytesSent()
}

// SetUserData sets some user data associated with the connection.
func (sc *ServerConn) SetUserData(v interface{}) {
	sc.userData = v
}

// UserData returns some user data associated with the connection.
func (sc *ServerConn) UserData() interface{} {
	return sc.userData
}

func (sc *ServerConn) ip() net.IP {
	return sc.remoteAddr.IP
}

func (sc *ServerConn) zone() string {
	return sc.remoteAddr.Zone
}

func (sc *ServerConn) run() {
	defer sc.s.wg.Done()
	defer close(sc.done)

	if h, ok := sc.s.Handler.(ServerHandlerOnConnOpen); ok {
		h.OnConnOpen(&ServerHandlerOnConnOpenCtx{
			Conn: sc,
		})
	}

	sc.conn = conn.NewConn(sc.bc)
	cr := &serverConnReader{
		sc: sc,
	}
	cr.initialize()

	err := sc.runInner()

	sc.ctxCancel()

	sc.nconn.Close()

	cr.wait()

	if sc.session != nil {
		sc.session.removeConn(sc)
	}

	sc.s.closeConn(sc)

	if h, ok := sc.s.Handler.(ServerHandlerOnConnClose); ok {
		h.OnConnClose(&ServerHandlerOnConnCloseCtx{
			Conn:  sc,
			Error: err,
		})
	}
}

func (sc *ServerConn) runInner() error {
	for {
		select {
		case req := <-sc.chReadRequest:
			req.res <- sc.handleRequestOuter(req.req)

		case err := <-sc.chReadError:
			return err

		case ss := <-sc.chRemoveSession:
			if sc.session == ss {
				sc.session = nil
			}

		case <-sc.ctx.Done():
			return liberrors.ErrServerTerminated{}
		}
	}
}

func (sc *ServerConn) handleRequestInner(req *base.Request) (*base.Response, error) {
	if cseq, ok := req.Header["CSeq"]; !ok || len(cseq) != 1 {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerCSeqMissing{}
	}

	if req.Method != base.Options && req.URL == nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerInvalidPath{}
	}

	sxID := getSessionID(req.Header)

	var path string
	var query string

	switch req.Method {
	case base.Describe, base.GetParameter, base.SetParameter:
		path, query = getPathAndQuery(req.URL, false)
	}

	switch req.Method {
	case base.Options:
		if sxID != "" {
			return sc.handleRequestInSession(sxID, req, false)
		}

		var methods []string
		if _, ok := sc.s.Handler.(ServerHandlerOnDescribe); ok {
			methods = append(methods, string(base.Describe))
		}
		if _, ok := sc.s.Handler.(ServerHandlerOnAnnounce); ok {
			methods = append(methods, string(base.Announce))
		}
		if _, ok := sc.s.Handler.(ServerHandlerOnSetup); ok {
			methods = append(methods, string(base.Setup))
		}
		if _, ok := sc.s.Handler.(ServerHandlerOnPlay); ok {
			methods = append(methods, string(base.Play))
		}
		if _, ok := sc.s.Handler.(ServerHandlerOnRecord); ok {
			methods = append(methods, string(base.Record))
		}
		if _, ok := sc.s.Handler.(ServerHandlerOnPause); ok {
			methods = append(methods, string(base.Pause))
		}
		methods = append(methods, string(base.GetParameter))
		if _, ok := sc.s.Handler.(ServerHandlerOnSetParameter); ok {
			methods = append(methods, string(base.SetParameter))
		}
		methods = append(methods, string(base.Teardown))

		return &base.Response{
			StatusCode: base.StatusOK,
			Header: base.Header{
				"Public": base.HeaderValue{strings.Join(methods, ", ")},
			},
		}, nil

	case base.Describe:
		if h, ok := sc.s.Handler.(ServerHandlerOnDescribe); ok {
			res, stream, err := h.OnDescribe(&ServerHandlerOnDescribeCtx{
				Conn:    sc,
				Request: req,
				Path:    path,
				Query:   query,
			})

			if res.StatusCode == base.StatusOK {
				if res.Header == nil {
					res.Header = make(base.Header)
				}

				res.Header["Content-Base"] = base.HeaderValue{req.URL.String() + "/"}
				res.Header["Content-Type"] = base.HeaderValue{"application/sdp"}

				// VLC uses multicast if the SDP contains a multicast address.
				// therefore, we introduce a special query (vlcmulticast) that allows
				// to return a SDP that contains a multicast address.
				multicast := false
				if sc.s.MulticastIPRange != "" {
					if q, err2 := gourl.ParseQuery(query); err2 == nil {
						if _, ok := q["vlcmulticast"]; ok {
							multicast = true
						}
					}
				}

				if stream != nil {
					byts, _ := serverSideDescription(stream.desc).Marshal(multicast)
					res.Body = byts
				}
			}

			return res, err
		}

	case base.Announce:
		if _, ok := sc.s.Handler.(ServerHandlerOnAnnounce); ok {
			return sc.handleRequestInSession(sxID, req, true)
		}

	case base.Setup:
		if _, ok := sc.s.Handler.(ServerHandlerOnSetup); ok {
			return sc.handleRequestInSession(sxID, req, true)
		}

	case base.Play:
		if sxID != "" {
			if _, ok := sc.s.Handler.(ServerHandlerOnPlay); ok {
				return sc.handleRequestInSession(sxID, req, false)
			}
		}

	case base.Record:
		if sxID != "" {
			if _, ok := sc.s.Handler.(ServerHandlerOnRecord); ok {
				return sc.handleRequestInSession(sxID, req, false)
			}
		}

	case base.Pause:
		if sxID != "" {
			if _, ok := sc.s.Handler.(ServerHandlerOnPause); ok {
				return sc.handleRequestInSession(sxID, req, false)
			}
		}

	case base.Teardown:
		if sxID != "" {
			return sc.handleRequestInSession(sxID, req, false)
		}

	case base.GetParameter:
		if sxID != "" {
			return sc.handleRequestInSession(sxID, req, false)
		}

		if h, ok := sc.s.Handler.(ServerHandlerOnGetParameter); ok {
			return h.OnGetParameter(&ServerHandlerOnGetParameterCtx{
				Conn:    sc,
				Request: req,
				Path:    path,
				Query:   query,
			})
		}

	case base.SetParameter:
		if sxID != "" {
			return sc.handleRequestInSession(sxID, req, false)
		}

		if h, ok := sc.s.Handler.(ServerHandlerOnSetParameter); ok {
			return h.OnSetParameter(&ServerHandlerOnSetParameterCtx{
				Conn:    sc,
				Request: req,
				Path:    path,
				Query:   query,
			})
		}
	}

	return &base.Response{
		StatusCode: base.StatusNotImplemented,
	}, nil
}

func (sc *ServerConn) handleRequestOuter(req *base.Request) error {
	if h, ok := sc.s.Handler.(ServerHandlerOnRequest); ok {
		h.OnRequest(sc, req)
	}

	res, err := sc.handleRequestInner(req)

	if res.Header == nil {
		res.Header = make(base.Header)
	}

	// add cseq
	var eerr liberrors.ErrServerCSeqMissing
	if !errors.As(err, &eerr) {
		res.Header["CSeq"] = req.Header["CSeq"]
	}

	// add server
	res.Header["Server"] = base.HeaderValue{"gortsplib"}

	if h, ok := sc.s.Handler.(ServerHandlerOnResponse); ok {
		h.OnResponse(sc, res)
	}

	sc.nconn.SetWriteDeadline(time.Now().Add(sc.s.WriteTimeout))
	err2 := sc.conn.WriteResponse(res)
	if err == nil && err2 != nil {
		err = err2
	}

	return err
}

func (sc *ServerConn) handleRequestInSession(
	sxID string,
	req *base.Request,
	create bool,
) (*base.Response, error) {
	// handle directly in Session
	if sc.session != nil {
		// session ID is optional in SETUP and ANNOUNCE requests, since
		// client may not have received the session ID yet due to multiple reasons:
		// * requests can be retries after code 301
		// * SETUP requests comes after ANNOUNCE response, that don't contain the session ID
		if sxID != "" {
			// the connection can't communicate with two sessions at once.
			if sxID != sc.session.secretID {
				return &base.Response{
					StatusCode: base.StatusBadRequest,
				}, liberrors.ErrServerLinkedToOtherSession{}
			}
		}

		cres := make(chan sessionRequestRes)
		sreq := sessionRequestReq{
			sc:     sc,
			req:    req,
			id:     sxID,
			create: create,
			res:    cres,
		}

		res, session, err := sc.session.handleRequest(sreq)
		sc.session = session
		return res, err
	}

	// otherwise, pass through Server
	cres := make(chan sessionRequestRes)
	sreq := sessionRequestReq{
		sc:     sc,
		req:    req,
		id:     sxID,
		create: create,
		res:    cres,
	}

	res, session, err := sc.s.handleRequest(sreq)
	sc.session = session
	return res, err
}

func (sc *ServerConn) removeSession(ss *ServerSession) {
	select {
	case sc.chRemoveSession <- ss:
	case <-sc.ctx.Done():
	}
}

func (sc *ServerConn) readRequest(req readReq) error {
	select {
	case sc.chReadRequest <- req:
		return <-req.res

	case <-sc.ctx.Done():
		return liberrors.ErrServerTerminated{}
	}
}

func (sc *ServerConn) readError(err error) {
	select {
	case sc.chReadError <- err:
	case <-sc.ctx.Done():
	}
}
