package tunnel

import (
	ex "deblocus/exception"
	"fmt"
	log "golang/glog"
	"net"
	"sync"
	"sync/atomic"
)

var client_sid int32

type Client struct {
	d5p           *D5Params
	ctlConn       *Conn
	token         []byte
	cipherFactory *CipherFactory
	lock          *sync.Mutex
	aliveTT       int32
	waitTk        *sync.Cond
	aborted       bool
}

func NewClient(d5p *D5Params, dhKeys *DHKeyPair, exitHandler CtlExitHandler) *Client {
	nego := new(d5CNegotiation)
	nego.D5Params = d5p
	nego.dhKeys = dhKeys
	nego.algoId = d5p.algoId
	ctlConn := nego.negotiate()
	log.Infof("Connected d5://%s.\n", d5p.d5ser)
	ctlConn.NoDelayAlive()
	me := &Client{
		d5p:     d5p,
		ctlConn: ctlConn,
		token:   nego.token,
		lock:    new(sync.Mutex),
	}
	me.waitTk = sync.NewCond(me.lock)
	me.cipherFactory = nego.cipherFactory
	go RControlThread(ctlConn, me.commandHandler, exitHandler)
	return me
}

func (this *Client) ClientServe(conn net.Conn) {
	var bconn *Conn
	var done bool
	defer func() {
		if !done {
			SafeClose(conn)
			SafeClose(bconn)
		}
		ex.CatchException(recover())
	}()

	if log.V(2) {
		log.Infoln("Request/socks5 from", conn.RemoteAddr().String())
	}
	s5 := S5Step1{conn: conn}
	s5.Handshake()
	if !s5.HandshakeAck() {
		target := s5.parseSocks5Request()
		if !s5.respondSocks5() {
			sid := atomic.AddInt32(&client_sid, 1)
			if log.V(1) {
				log.Infof("SID#%X connect to %s\n", sid, target)
			}
			bconn = this.createTunnel(sid, s5.target)
			atomic.AddInt32(&this.aliveTT, 1)
			go Pipe(conn, bconn, sid)
			Pipe(bconn, conn, sid)
			atomic.AddInt32(&this.aliveTT, -1)
			done = true
		}
	}
}

func (this *Client) createTunnel(sid int32, target []byte) *Conn {
	conn, err := net.Dial("tcp", this.d5p.d5ser)
	ThrowErr(err)
	buf := make([]byte, DMLEN)
	token := this.getToken(sid)
	copy(buf, token)
	copy(buf[TT_TOKEN_OFFSET:], token) // TT_TOKEN_OFFSET
	copy(buf[TT_TOKEN_OFFSET+SzTk:], target)
	buf[SzTk] = token[SzTk-1]
	buf[SzTk+1] = D5
	cipher := this.cipherFactory.NewCipher()
	cipher.encrypt(buf[TT_TOKEN_OFFSET:], buf[TT_TOKEN_OFFSET:])
	_, err = conn.Write(buf)
	ThrowErr(err)
	return NewConn(conn.(*net.TCPConn), cipher)
}

func (t *Client) Stats() string {
	return fmt.Sprintf("Client:Stats To-%s TT=%d TM=%d", t.d5p.d5ser,
		atomic.LoadInt32(&t.aliveTT), len(t.token)/SzTk)
}

func (this *Client) getToken(sid int32) []byte {
	defer func() {
		this.lock.Unlock()
		tlen := len(this.token) / SzTk
		if tlen < 8 && !this.aborted {
			if log.V(2) {
				log.Infof("Request new tokens. tokenPool=%d\n", tlen)
			}
			go postCommand(this.ctlConn, TOKEN_REQUEST, nil)
		}
	}()
	this.lock.Lock()
	for len(this.token) < SzTk {
		if log.V(2) {
			log.Infof("SID#%X waiting for token. May be the requests comes too fast, or the responding slowly.\n", sid)
		}
		this.waitTk.Wait()
		if this.aborted {
			panic("Abandon the request beacause of the tunSession was aborted.")
		}
	}
	token := this.token[:SzTk]
	this.token = this.token[SzTk:]
	if log.V(2) {
		tlen := len(this.token) / SzTk
		log.Infof("SID#%X take token=[%x] tokenPool=%d\n", sid, token, tlen)
	}
	return token
}

func (this *Client) putTokens(tokens []byte) {
	defer this.lock.Unlock()
	this.lock.Lock()
	this.token = append(this.token, tokens...)
	this.waitTk.Broadcast()
	log.Infof("Recv tokens=%d tokens_pool=%d\n", len(tokens)/SzTk, len(this.token)/SzTk)
}

func (this *Client) commandHandler(cmd byte, args []byte) {
	switch cmd {
	case TOKEN_REPLY:
		this.putTokens(args)
	default:
		log.Warningf("Unrecognized command=%x packet=[% x]\n", cmd, args)
	}
}
