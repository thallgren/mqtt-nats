package bridge

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
	"github.com/tada/mqtt-nats/jsonstream"
	"github.com/tada/mqtt-nats/logger"
	"github.com/tada/mqtt-nats/mqtt"
	"github.com/tada/mqtt-nats/mqtt/pkg"
	"github.com/tada/mqtt-nats/pio"
)

type Server interface {
	pkg.IDManager
	SessionManager() SessionManager
	ManageClient(c Client)
	NatsConn(creds *pkg.Credentials) (*nats.Conn, error)
	HandleRetain(pp *pkg.Publish) *pkg.Publish
	PublishMatching(sp *pkg.Subscribe, c Client)
	PublishWill(will *pkg.Will, creds *pkg.Credentials) error
}

type Bridge interface {
	Server
	Done() <-chan bool
	Restart(ready *sync.WaitGroup) error
	Serve(ready *sync.WaitGroup) error
	ServeClient(conn net.Conn)
	Shutdown() error
}

type server struct {
	logger.Logger
	pkg.IDManager
	opts            *Options
	session         Session
	retainedPackets *retained
	sm              SessionManager
	natsConn        *nats.Conn // servers NATS connection
	clients         []Client
	clientWG        sync.WaitGroup
	clientLock      sync.RWMutex
	trackAckLock    sync.RWMutex
	pubAcks         map[uint16]*natsPub // will be republished until ack arrives from nats
	pubAckTimeout   time.Duration
	pubAckTimer     *time.Timer
	done            chan bool
	signals         chan os.Signal
}

func New(opts *Options, logger logger.Logger) (Bridge, error) {
	s := &server{
		Logger:    logger,
		IDManager: pkg.NewIDManager(),
		opts:      opts,
		retainedPackets: &retained{
			msgs: make(map[string]*pkg.Publish),
		},
		pubAckTimeout: time.Duration(opts.RepeatRate) * time.Millisecond,
		sm:            &sm{m: make(map[string]Session, 37)},
		signals:       make(chan os.Signal, 1),
	}

	s.session = s.sm.Create(`mqtt-nats-` + nuid.Next())
	var err error
	if opts.StoragePath != "" {
		err = s.load(opts.StoragePath)
	}
	return s, err
}

func (s *server) Restart(ready *sync.WaitGroup) error {
	err := s.Shutdown()
	if err == nil && s.opts.StoragePath != "" {
		err = s.load(s.opts.StoragePath)
	}
	if err == nil {
		err = s.Serve(ready)
	} else {
		ready.Done()
	}
	return err
}

func (s *server) Shutdown() error {
	s.signals <- syscall.SIGINT
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		return errors.New("timeout during bridge shutdown")
	}
	return nil
}

// Done will be closed when the shutdown is complete
func (s *server) Done() <-chan bool {
	return s.done
}

func (s *server) bootUp(ready *sync.WaitGroup) (net.Listener, error) {
	if ready != nil {
		defer ready.Done()
	}

	listener, err := getTCPListener(s.opts)
	if err != nil {
		return nil, err
	}

	if s.opts.RetainedRequestTopic != "" {
		if err = s.startRetainedRequestHandler(); err != nil {
			return nil, err
		}
	}

	s.done = make(chan bool, 1)
	return listener, nil
}

func (s *server) Serve(ready *sync.WaitGroup) error {
	listener, err := s.bootUp(ready)
	if err != nil {
		return err
	}

	signal.Notify(s.signals, syscall.SIGINT, syscall.SIGTERM)
	shuttingDown := false

	go func() {
		sig := <-s.signals
		s.Debug("trapped signal", sig)
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			s.Debug("mqtt-nats is shutting down")
			shuttingDown = true
			_ = listener.Close()
		}
		// TODO: Add signal to reload config.
	}()

	for {
		mqttConn, err := listener.Accept()
		if err != nil {
			if shuttingDown {
				// error due to close of listener
				break
			}
			s.Error(err)
		} else {
			go s.ServeClient(mqttConn)
		}
	}
	return s.drainAndShutdown()
}

func (s *server) drainAndShutdown() error {
	s.Debug("waiting for clients to drain")
	s.clientLock.Lock()
	clients := s.clients
	s.clients = nil
	s.clientLock.Unlock()

	for i := range clients {
		clients[i].SetDisconnected(nil)
	}
	s.clientWG.Wait()
	s.Debug("client drain complete")

	s.trackAckLock.Lock()
	if s.pubAckTimer != nil {
		s.pubAckTimer.Stop()
		s.pubAckTimer = nil
	}
	s.trackAckLock.Unlock()

	if s.natsConn != nil {
		s.natsConn.Close()
		s.natsConn = nil
	}

	var err error
	if s.opts.StoragePath != "" {
		err = s.persist(s.opts.StoragePath)
	}
	close(s.done)
	return err
}

func (s *server) startRetainedRequestHandler() error {
	conn, err := s.serverNatsConn()
	if err != nil {
		return err
	}
	_, err = conn.Subscribe(s.opts.RetainedRequestTopic, s.handleRetainedRequest)
	return err
}

func (s *server) handleRetainedRequest(m *nats.Msg) {
	pps, qs := s.retainedPackets.messagesMatchingRetainRequest(m)
	var err error
	if len(pps) == 0 {
		err = m.Respond([]byte("[]"))
	} else {
		err = pio.Catch(func() error {
			qos := byte(0)
			buf := &bytes.Buffer{}
			pio.WriteByte('[', buf)
			for i := range pps {
				pp := pps[i]
				if qs[0] > qos {
					qos = qs[0]
				}
				if i > 0 {
					pio.WriteByte(',', buf)
				}
				pio.WriteString(`{"subject":`, buf)
				jsonstream.WriteString(mqtt.ToNATS(pp.TopicName()), buf)
				if pkg.IsPrintableASCII(pp.Payload()) {
					pio.WriteString(`,"payload":`, buf)
					jsonstream.WriteString(string(pp.Payload()), buf)
				} else {
					pio.WriteString(`,"payloadEnc":`, buf)
					jsonstream.WriteString(base64.StdEncoding.EncodeToString(pp.Payload()), buf)
				}
				pio.WriteByte('}', buf)
			}
			pio.WriteByte(']', buf)
			return m.Respond(buf.Bytes())
		})
	}
	if err != nil {
		s.Error("NATS publish of retained messages failed", err)
	}
}

func (s *server) natsOptions(creds *pkg.Credentials) (*nats.Options, error) {
	opts := nats.GetDefaultOptions()
	opts.Servers = strings.Split(s.opts.NATSUrls, ",")
	optFuncs := s.opts.NATSOpts
	for i := range optFuncs {
		if err := optFuncs[i](&opts); err != nil {
			return nil, err
		}
	}
	if creds != nil {
		opts.User = creds.User
		if creds.Password != nil {
			// TODO: Handle password correctly, definitions differ
			opts.Password = string(creds.Password)
		}
	}
	return &opts, nil
}

func (s *server) PublishWill(will *pkg.Will, creds *pkg.Credentials) error {
	id := uint16(0)
	qos := will.QoS
	if qos > 0 {
		id = s.NextFreePacketID()
	}
	pp := pkg.NewPublish2(id, will.Topic, will.Message, qos, false, will.Retain)
	nc, err := s.NatsConn(creds)
	if err == nil {
		defer nc.Close()
		natsSubj := mqtt.ToNATS(will.Topic)
		if qos == 0 {
			err = nc.Publish(natsSubj, will.Message)
		} else {
			// use client id and packet id to form a reply subject
			replyTo := NewReplyTopic(s.session, pp).String()
			err = nc.PublishRequest(natsSubj, replyTo, will.Message)
		}
		if err == nil {
			if will.Retain {
				s.HandleRetain(pp)
			} else if qos > 0 {
				pp.SetDup()
				s.trackAckReceived(pp, creds)
			}
		}
	}
	return err
}

// trackAckReceived is used when a publish using QoS > 0 originates from this server and needs to be
// maintained until an ack is received. One example of when this happens is when a client will has QoS > 0
func (s *server) trackAckReceived(pp *pkg.Publish, creds *pkg.Credentials) {
	s.Debug("track", pp)
	s.trackAckLock.Lock()
	np := natsPub{pp: pp, creds: creds}
	if s.pubAcks == nil {
		s.pubAcks = make(map[uint16]*natsPub)
		s.pubAckTimer = time.AfterFunc(s.pubAckTimeout, s.ackCheckTick)
	}
	s.pubAcks[pp.ID()] = &np
	s.trackAckLock.Unlock()
}

func (s *server) ackCheckTick() {
	s.pubAckTimer.Reset(s.pubAckTimeout)
	for _, np := range s.awaitsAckSnapshot() {
		s.republish(np)
	}
}

func (s *server) awaitsAckSnapshot() []*natsPub {
	i := 0
	s.trackAckLock.RLock()
	nps := make([]*natsPub, len(s.pubAcks))
	for _, np := range s.pubAcks {
		nps[i] = np
		i++
	}
	s.trackAckLock.RUnlock()
	sort.Slice(nps, func(i, j int) bool { return nps[i].pp.ID() < nps[j].pp.ID() })
	return nps
}

func (s *server) NatsConn(creds *pkg.Credentials) (*nats.Conn, error) {
	var nc *nats.Conn
	natsOpts, err := s.natsOptions(creds)
	if err == nil {
		nc, err = natsOpts.Connect()
	}
	return nc, err
}

func (s *server) serverNatsConn() (*nats.Conn, error) {
	var err error
	if s.natsConn == nil {
		s.natsConn, err = s.NatsConn(nil)
	}
	return s.natsConn, err
}

func (s *server) republish(np *natsPub) {
	var (
		conn *nats.Conn
		err  error
	)
	if np.creds == nil {
		conn, err = s.serverNatsConn()
	} else if conn, err = s.NatsConn(np.creds); err == nil {
		// temporary connection with credentials from the client where the
		// message originated, must be closed on function return
		defer conn.Close()
	}
	if err != nil {
		s.Error(err)
		return
	}

	pp := np.pp
	replyTo := NewReplyTopic(s.session, pp).String()
	sub, err := conn.SubscribeSync(replyTo)
	if err != nil {
		s.Error(err)
		return
	}

	s.Debug("republish", pp)
	if err = conn.PublishRequest(mqtt.ToNATS(pp.TopicName()), replyTo, pp.Payload()); err != nil {
		s.Error(err)
		return
	}

	if _, err = sub.NextMsg(2 * time.Second); err == nil {
		s.Debug("ack", pp.ID())
		s.trackAckLock.Lock()
		if s.pubAcks != nil {
			delete(s.pubAcks, pp.ID())
			if len(s.pubAcks) == 0 {
				s.pubAckTimer.Stop()
				s.pubAckTimer = nil
				s.pubAcks = nil
			}
		}
		s.trackAckLock.Unlock()
	} else if err != nats.ErrTimeout {
		s.Error(err)
	}
}

func (s *server) MarshalToJSON(w io.Writer) {
	pio.WriteString(`{"ts":`, w)
	jsonstream.WriteString(time.Now().Format(time.RFC3339), w)
	pio.WriteString(`,"id":`, w)
	jsonstream.WriteString(s.session.ClientID(), w)
	pio.WriteString(`,"idm":`, w)
	s.IDManager.MarshalToJSON(w)
	pio.WriteString(`,"sm":`, w)
	s.sm.MarshalToJSON(w)
	if !s.retainedPackets.Empty() {
		pio.WriteString(`,"retained":`, w)
		s.retainedPackets.MarshalToJSON(w)
	}
	trk := s.awaitsAckSnapshot()
	if len(trk) > 0 {
		pio.WriteString(`,"pubacks":[`, w)
		for i := range trk {
			if i > 0 {
				pio.WriteByte(',', w)
			}
			trk[i].MarshalToJSON(w)
		}
		pio.WriteByte(']', w)
	}
	pio.WriteByte('}', w)
}

func (s *server) UnmarshalFromJSON(js *json.Decoder, t json.Token) {
	jsonstream.AssertDelimToken(t, '{')
	var (
		id        string
		ackTracks map[uint16]*natsPub
	)
	for {
		k, ok := jsonstream.AssertStringOrEnd(js, '}')
		if !ok {
			break
		}
		switch k {
		case "id":
			id = jsonstream.AssertString(js)
		case "idm":
			jsonstream.AssertConsumer(js, s.IDManager)
		case "sm":
			jsonstream.AssertConsumer(js, s.sm)
		case "retained":
			jsonstream.AssertConsumer(js, s.retainedPackets)
		case "pubacks":
			jsonstream.AssertDelim(js, '[')
			ackTracks = make(map[uint16]*natsPub)
			for {
				np := &natsPub{}
				if !jsonstream.AssertConsumerOrEnd(js, np, ']') {
					break
				}
				ackTracks[np.pp.ID()] = np
			}
		}
	}
	if id != "" {
		s.session = s.sm.Get(id)
	}
	if len(ackTracks) > 0 {
		s.pubAcks = ackTracks
		s.pubAckTimer = time.AfterFunc(s.pubAckTimeout, s.ackCheckTick)
	}
}

func (s *server) HandleRetain(pp *pkg.Publish) *pkg.Publish {
	if pp.Retain() {
		rt := s.retainedPackets
		if len(pp.Payload()) == 0 {
			if rt.drop(pp.TopicName()) {
				s.Debug("deleted retained message", pp)
			}
			pp.ResetRetain()
		} else if rt.add(pp) {
			s.Debug("added retained message", pp)
		}
	}
	return pp
}

func (s *server) PublishMatching(ps *pkg.Subscribe, c Client) {
	s.retainedPackets.publishMatching(ps, c)
}

func (s *server) ServeClient(conn net.Conn) {
	s.clientWG.Add(1)
	defer s.clientWG.Done()
	c := NewClient(s, s, conn)
	c.Serve()
	s.unmanageClient(c)
}

// ManageClient adds the client to the list of clients managed by the server
func (s *server) ManageClient(c Client) {
	s.clientLock.Lock()
	s.clients = append(s.clients, c)
	s.clientLock.Unlock()
}

// unmanageClient removes the client from the list of clients managed by the server
func (s *server) unmanageClient(c Client) {
	s.clientLock.Lock()
	cs := s.clients
	ln := len(cs) - 1
	for i := 0; i <= ln; i++ {
		if c == cs[i] {
			cs[i] = cs[ln]
			cs[ln] = nil
			s.clients = cs[:ln]
			break
		}
	}
	s.clientLock.Unlock()
}

func (s *server) load(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0600)
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return err
	}
	defer func() {
		err = f.Close()
	}()
	return pio.Catch(func() error {
		dc := json.NewDecoder(bufio.NewReader(f))
		dc.UseNumber()
		jsonstream.AssertConsumer(dc, s)
		s.Debug("State loaded from", path)
		return nil
	})
}

func (s *server) persist(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() {
		err = f.Close()
	}()
	return pio.Catch(func() error {
		w := bufio.NewWriter(f)
		s.MarshalToJSON(w)
		s.Debug("server State persisted to ", path)
		return w.Flush()
	})
}

func getTCPListener(opts *Options) (net.Listener, error) {
	ps := `:` + strconv.Itoa(opts.Port)
	if !opts.TLS {
		return net.Listen("tcp", ps)
	}

	// Load mandatory server key pair for bridge
	cer, err := tls.LoadX509KeyPair(opts.TLSCert, opts.TLSKey)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cer}}

	if opts.TLSVerify {
		cfg.ClientAuth = tls.RequestClientCert
	}

	if opts.TLSCaCert != `` {
		roots := x509.NewCertPool()
		var caPem []byte
		if caPem, err = ioutil.ReadFile(opts.TLSCaCert); err != nil {
			return nil, err
		}
		if ok := roots.AppendCertsFromPEM(caPem); !ok {
			return nil, fmt.Errorf("failed to parse root certificate in file: %s", opts.TLSCaCert)
		}
		cfg.ClientCAs = roots
	}

	return tls.Listen(`tcp`, ps, cfg)
}

func (s *server) SessionManager() SessionManager {
	return s.sm
}
