package p2p

import (
	"fmt"
	"net"
	"time"
	"io"
	"bufio"
	"encoding/binary"
	"os/exec"
	"strings"

	cmn "github.com/tendermint/tendermint/libs/common"
	"github.com/tendermint/tendermint/libs/log"

	tmconn "github.com/tendermint/tendermint/p2p/conn"
)

const metricsTickerDuration = 10 * time.Second

// Peer is an interface representing a peer connected on a reactor.
type Peer interface {
	cmn.Service
	FlushStop()

	ID() ID               // peer's cryptographic ID
	RemoteIP() net.IP     // remote IP of the connection
	RemoteAddr() net.Addr // remote address of the connection

	IsOutbound() bool   // did we dial the peer
	IsPersistent() bool // do we redial this peer when we disconnect

	CloseConn() error // close original connection

	NodeInfo() NodeInfo // peer's info
	Status() tmconn.ConnectionStatus
	SocketAddr() *NetAddress // actual address of the socket

	Send(byte, []byte) bool
	TrySend(byte, []byte) bool

	Set(string, interface{})
	Get(string) interface{}
}

//----------------------------------------------------------

// peerConn contains the raw connection and its config.
type peerConn struct {
	outbound   bool
	persistent bool
	conn       net.Conn // source connection

	socketAddr *NetAddress

	// cached RemoteIP()
	ip net.IP
}

func newPeerConn(
	outbound, persistent bool,
	conn net.Conn,
	socketAddr *NetAddress,
) peerConn {

	return peerConn{
		outbound:   outbound,
		persistent: persistent,
		conn:       conn,
		socketAddr: socketAddr,
	}
}

// ID only exists for SecretConnection.
// NOTE: Will panic if conn is not *SecretConnection.
func (pc peerConn) ID() ID {
	return PubKeyToID(pc.conn.(*tmconn.SecretConnection).RemotePubKey())
}

// Return the IP from the connection RemoteAddr
func (pc peerConn) RemoteIP() net.IP {
	if pc.ip != nil {
		return pc.ip
	}

	host, _, err := net.SplitHostPort(pc.conn.RemoteAddr().String())
	if err != nil {
		panic(err)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		panic(err)
	}

	pc.ip = ips[0]

	return pc.ip
}

// peer implements Peer.
//
// Before using a peer, you will need to perform a handshake on connection.
type peer struct {
	cmn.BaseService

	// raw peerConn and the multiplex connection
	peerConn
	mconn *tmconn.MConnection

	// peer's node info and the channel it knows about
	// channels = nodeInfo.Channels
	// cached to avoid copying nodeInfo in hasChannel
	nodeInfo NodeInfo
	channels []byte

	// User data
	Data *cmn.CMap

	metrics       *Metrics
	metricsTicker *time.Ticker
}

type PeerOption func(*peer)

func newPeer(
	pc peerConn,
	mConfig tmconn.MConnConfig,
	nodeInfo NodeInfo,
	reactorsByCh map[byte]Reactor,
	chDescs []*tmconn.ChannelDescriptor,
	onPeerError func(Peer, interface{}),
	options ...PeerOption,
) *peer {
	p := &peer{
		peerConn:      pc,
		nodeInfo:      nodeInfo,
		channels:      nodeInfo.(DefaultNodeInfo).Channels, // TODO
		Data:          cmn.NewCMap(),
		metricsTicker: time.NewTicker(metricsTickerDuration),
		metrics:       NopMetrics(),
	}

	p.mconn = createMConnection(
		pc.conn,
		p,
		reactorsByCh,
		chDescs,
		onPeerError,
		mConfig,
	)
	p.BaseService = *cmn.NewBaseService(nil, "Peer", p)
	for _, option := range options {
		option(p)
	}

	return p
}

// String representation.
func (p *peer) String() string {
	if p.outbound {
		return fmt.Sprintf("Peer{%v %v out}", p.mconn, p.ID())
	}

	return fmt.Sprintf("Peer{%v %v in}", p.mconn, p.ID())
}

//---------------------------------------------------
// Implements cmn.Service

// SetLogger implements BaseService.
func (p *peer) SetLogger(l log.Logger) {
	p.Logger = l
	p.mconn.SetLogger(l)
}

// OnStart implements BaseService.
func (p *peer) OnStart() error {
	if err := p.BaseService.OnStart(); err != nil {
		return err
	}

	if err := p.mconn.Start(); err != nil {
		return err
	}

	go p.metricsReporter()
	return nil
}

// FlushStop mimics OnStop but additionally ensures that all successful
// .Send() calls will get flushed before closing the connection.
// NOTE: it is not safe to call this method more than once.
func (p *peer) FlushStop() {
	p.metricsTicker.Stop()
	p.BaseService.OnStop()
	p.mconn.FlushStop() // stop everything and close the conn
}

// OnStop implements BaseService.
func (p *peer) OnStop() {
	p.metricsTicker.Stop()
	p.BaseService.OnStop()
	p.mconn.Stop() // stop everything and close the conn
}

//---------------------------------------------------
// Implements Peer

// ID returns the peer's ID - the hex encoded hash of its pubkey.
func (p *peer) ID() ID {
	return p.nodeInfo.ID()
}

// IsOutbound returns true if the connection is outbound, false otherwise.
func (p *peer) IsOutbound() bool {
	return p.peerConn.outbound
}

// IsPersistent returns true if the peer is persitent, false otherwise.
func (p *peer) IsPersistent() bool {
	return p.peerConn.persistent
}

// NodeInfo returns a copy of the peer's NodeInfo.
func (p *peer) NodeInfo() NodeInfo {
	return p.nodeInfo
}

// SocketAddr returns the address of the socket.
// For outbound peers, it's the address dialed (after DNS resolution).
// For inbound peers, it's the address returned by the underlying connection
// (not what's reported in the peer's NodeInfo).
func (p *peer) SocketAddr() *NetAddress {
	return p.peerConn.socketAddr
}

// Status returns the peer's ConnectionStatus.
func (p *peer) Status() tmconn.ConnectionStatus {
	return p.mconn.Status()
}

// Send msg bytes to the channel identified by chID byte. Returns false if the
// send queue is full after timeout, specified by MConnection.
func (p *peer) Send(chID byte, msgBytes []byte) bool {
	if !p.IsRunning() {
		// see Switch#Broadcast, where we fetch the list of peers and loop over
		// them - while we're looping, one peer may be removed and stopped.
		return false
	} else if !p.hasChannel(chID) {
		return false
	}
	res := p.mconn.Send(chID, msgBytes)
	if res {
		labels := []string{
			"peer_id", string(p.ID()),
			"chID", fmt.Sprintf("%#x", chID),
		}
		p.metrics.PeerSendBytesTotal.With(labels...).Add(float64(len(msgBytes)))
	}
	return res
}

// TrySend msg bytes to the channel identified by chID byte. Immediately returns
// false if the send queue is full.
func (p *peer) TrySend(chID byte, msgBytes []byte) bool {
	if !p.IsRunning() {
		return false
	} else if !p.hasChannel(chID) {
		return false
	}
	res := p.mconn.TrySend(chID, msgBytes)
	if res {
		labels := []string{
			"peer_id", string(p.ID()),
			"chID", fmt.Sprintf("%#x", chID),
		}
		p.metrics.PeerSendBytesTotal.With(labels...).Add(float64(len(msgBytes)))
	}
	return res
}

// Get the data for a given key.
func (p *peer) Get(key string) interface{} {
	return p.Data.Get(key)
}

// Set sets the data for the given key.
func (p *peer) Set(key string, data interface{}) {
	p.Data.Set(key, data)
}

// hasChannel returns true if the peer reported
// knowing about the given chID.
func (p *peer) hasChannel(chID byte) bool {
	for _, ch := range p.channels {
		if ch == chID {
			return true
		}
	}
	// NOTE: probably will want to remove this
	// but could be helpful while the feature is new
	p.Logger.Debug(
		"Unknown channel for peer",
		"channel",
		chID,
		"channels",
		p.channels,
	)
	return false
}

// CloseConn closes original connection. Used for cleaning up in cases where the peer had not been started at all.
func (p *peer) CloseConn() error {
	return p.peerConn.conn.Close()
}

//---------------------------------------------------
// methods only used for testing
// TODO: can we remove these?

// CloseConn closes the underlying connection
func (pc *peerConn) CloseConn() {
	pc.conn.Close() // nolint: errcheck
}

// RemoteAddr returns peer's remote network address.
func (p *peer) RemoteAddr() net.Addr {
	return p.peerConn.conn.RemoteAddr()
}

// CanSend returns true if the send queue is not full, false otherwise.
func (p *peer) CanSend(chID byte) bool {
	if !p.IsRunning() {
		return false
	}
	return p.mconn.CanSend(chID)
}

//---------------------------------------------------

func PeerMetrics(metrics *Metrics) PeerOption {
	return func(p *peer) {
		p.metrics = metrics
	}
}

func (p *peer) metricsReporter() {
	for {
		select {
		case <-p.metricsTicker.C:
			status := p.mconn.Status()
			var sendQueueSize float64
			for _, chStatus := range status.Channels {
				sendQueueSize += float64(chStatus.SendQueueSize)
			}

			p.metrics.PeerPendingSendBytes.With("peer_id", string(p.ID())).Set(sendQueueSize)
		case <-p.Quit():
			return
		}
	}
}

//------------------------------------------------------------------
// helper funcs

func createMConnection(
	conn net.Conn,
	p *peer,
	reactorsByCh map[byte]Reactor,
	chDescs []*tmconn.ChannelDescriptor,
	onPeerError func(Peer, interface{}),
	config tmconn.MConnConfig,
) *tmconn.MConnection {

	onReceive := func(chID byte, msgBytes []byte) {
		reactor := reactorsByCh[chID]
		if reactor == nil {
			// Note that its ok to panic here as it's caught in the conn._recover,
			// which does onPeerError.
			panic(fmt.Sprintf("Unknown channel %X", chID))
		}
		labels := []string{
			"peer_id", string(p.ID()),
			"chID", fmt.Sprintf("%#x", chID),
		}
		p.metrics.PeerReceiveBytesTotal.With(labels...).Add(float64(len(msgBytes)))
		reactor.Receive(chID, p, msgBytes)
	}

	onError := func(r interface{}) {
		onPeerError(p, r)
	}

	return tmconn.NewMConnectionWithConfig(
		conn,
		chDescs,
		onReceive,
		onError,
		config,
	)
}







// Blockchain Reactor Events (the input to the state machine)
type recvState uint

const (
 // message type events
 lengthWait = iota + 1
 messageWait
)

// marlinPeer implements Peer.
type marlinPeer struct {
 cmn.BaseService

 // raw peerConn and the multiplex connection
 peerConn

 // peer's node info and the channel it knows about
 // channels = nodeInfo.Channels
 // cached to avoid copying nodeInfo in hasChannel
 nodeInfo NodeInfo
 channels []byte

 // User data
 Data *cmn.CMap

 metrics       *Metrics
 metricsTicker *time.Ticker

 bufConnReader *bufio.Reader
 bufConnWriter *bufio.Writer

 peerRecvState uint
 bytesPending  uint64

 rctByCh map[byte]Reactor

 // send          chan struct{}
 // sendQueue     chan []byte
 // sendQueueSize int32 // atomic.

 // recving       []byte
 // sending       []byte
}

type MarlinPeerOption func(*marlinPeer)

func newMarlinPeer(
 pc peerConn,
 nodeInfo NodeInfo,
 reactorsByCh map[byte]Reactor,
 chDescs []*tmconn.ChannelDescriptor,
 onPeerError func(Peer, interface{}),
 options ...MarlinPeerOption,
) *marlinPeer {
 p := &marlinPeer{
   peerConn:      pc,
   nodeInfo:      nodeInfo,
   channels:      nodeInfo.(DefaultNodeInfo).Channels, // TODO
   Data:          cmn.NewCMap(),
   metricsTicker: time.NewTicker(metricsTickerDuration),
   metrics:       NopMetrics(),
   bufConnReader: bufio.NewReaderSize(pc.conn, 1024),
   bufConnWriter: bufio.NewWriterSize(pc.conn, 65536),
   peerRecvState: lengthWait,
   bytesPending:  8,
   rctByCh:       make(map[byte]Reactor),
 }

 for k, v := range reactorsByCh {
   p.rctByCh[k] = v
 }


 p.BaseService = *cmn.NewBaseService(nil, "MarlinPeer", p)
 for _, option := range options {
   option(p)
 }

 return p
}

// String representation.
func (p *marlinPeer) String() string {
 if p.outbound {
   return fmt.Sprintf("Peer{%v out}", p.ID())
 }

 return fmt.Sprintf("Peer{%v in}", p.ID())
}

//---------------------------------------------------
// Implements service.Service

// SetLogger implements BaseService.
func (p *marlinPeer) SetLogger(l log.Logger) {
 p.Logger = l
}

// OnStart implements BaseService.
func (p *marlinPeer) OnStart() error {
 if err := p.BaseService.OnStart(); err != nil {
   return err
 }

 // go p.sendRoutine()
 go p.recvRoutine()
 return nil
}

// FlushStop mimics OnStop but additionally ensures that all successful
// .Send() calls will get flushed before closing the connection.
// NOTE: it is not safe to call this method more than once.
func (p *marlinPeer) FlushStop() {
 p.BaseService.OnStop()
 p.FlushStop() // stop everything and close the conn
}

// OnStop implements BaseService.
func (p *marlinPeer) OnStop() {
 p.BaseService.OnStop()
}

//---------------------------------------------------
// Implements Peer

// ID returns the peer's ID - the hex encoded hash of its pubkey.
func (p *marlinPeer) ID() ID {
 return p.nodeInfo.ID()
}

// IsOutbound returns true if the connection is outbound, false otherwise.
func (p *marlinPeer) IsOutbound() bool {
 return p.peerConn.outbound
}

// IsPersistent returns true if the peer is persitent, false otherwise.
func (p *marlinPeer) IsPersistent() bool {
 return p.peerConn.persistent
}

// NodeInfo returns a copy of the peer's NodeInfo.
func (p *marlinPeer) NodeInfo() NodeInfo {
 return p.nodeInfo
}

// SocketAddr returns the address of the socket.
// For outbound peers, it's the address dialed (after DNS resolution).
// For inbound peers, it's the address returned by the underlying connection
// (not what's reported in the peer's NodeInfo).
func (p *marlinPeer) SocketAddr() *NetAddress {
 return p.peerConn.socketAddr
}

// Status returns the peer's ConnectionStatus.
func (p *marlinPeer) Status() tmconn.ConnectionStatus {
 var status tmconn.ConnectionStatus
 return status
}

// Send msg bytes to the channel identified by chID byte. Returns false if the
// send queue is full after timeout, specified by MConnection.
func (p *marlinPeer) Send(chID byte, msgBytes []byte) bool {
 if !p.SenseMarlinPortFixIfNeeded() {
   p.Logger.Error("Could not sense marlinport")
    return false
 }

 chIDLength := 1
 length := chIDLength + len(msgBytes)

 var err error

 // write 8 bit length
 buf := make([]byte, 8)
 binary.BigEndian.PutUint64(buf, uint64(length))
 _, err = p.bufConnWriter.Write(buf)

 if err != nil {
   p.Logger.Debug("Failed to write prefixed length")
   return false
 }

 // write channel id
 err = p.bufConnWriter.WriteByte(byte(chID))
 if err != nil {
   p.Logger.Debug("Failed to write channel")
   return false
 }

 // write the message buffer
 _, err = p.bufConnWriter.Write(msgBytes)
 p.bufConnWriter.Flush()

 if (err != nil) {
   p.Logger.Debug("Failed to write prefixed length")
   return false
 }

 return true
}

func (p *marlinPeer) ReadFrame() ([]byte, error) {
 var err error

 lenBuf, frameLength, err := p.getFrameLength()
 if err != nil {
   return nil, err
 }

 chIDLength := 1
 chID := make([]byte, chIDLength)
 _, err = io.ReadFull(p.bufConnReader, chID)
 if err != nil {
   return nil, err
 }

 // real message length
 msgLength := int(frameLength) - chIDLength
 msg := make([]byte, msgLength)
 _, err = io.ReadFull(p.bufConnReader, msg)
 if err != nil {
   return nil, err
 }

 fullMessage := make([]byte, len(lenBuf)+ int(frameLength))
 copy(fullMessage, lenBuf)
 copy(fullMessage[len(lenBuf):], chID)
 copy(fullMessage[len(lenBuf) + chIDLength :], msg)

 return fullMessage, nil
}


func (p *marlinPeer) getFrameLength() (lenBuf []byte, n uint64, err error) {

 lenBuf = make([]byte, 8)

 _, err = p.bufConnReader.Read(lenBuf)
 if err != nil {
   return nil, 0, err
 }

 return lenBuf, uint64(binary.BigEndian.Uint64(lenBuf)), nil
}

func (p *marlinPeer) readMessage() ([]byte, error) {
 var err error
 if (p.peerRecvState == lengthWait) {
   lenBuf := make([]byte, 8)
   _, err = p.bufConnReader.Read(lenBuf)

   if err != nil {
     return nil, err
   }

   p.peerRecvState = messageWait
   p.bytesPending = uint64(binary.BigEndian.Uint64(lenBuf))
 }

 if (p.peerRecvState == messageWait) {
   // real message length
   frameLength := p.bytesPending
   frame := make([]byte, frameLength)
   _, err = io.ReadFull(p.bufConnReader, frame)
   if err != nil {
     return nil, err
   }

   p.peerRecvState = lengthWait
   p.bytesPending = 8
   return frame, nil
 }

 return nil, err
}

func (p *marlinPeer) recvRoutine() {

//FOR_LOOP:
 for {
   if !p.SenseMarlinPortFixIfNeeded() {
     p.Logger.Error("Could not sense marlinport")
     continue
   }

   fullMessage, err := p.readMessage()

   if err != nil {

   } else {
     // fmt.Printf("frame received %d\n", len(fullMessage));

     chID := fullMessage[0];
     msgBytes := fullMessage[1:];

     reactor := p.rctByCh[chID]
     if reactor == nil {
       // Note that its ok to panic here as it's caught in the conn._recover,
       // which does onPeerError.
       panic(fmt.Sprintf("Unknown channel %X", chID))
     }
     reactor.Receive(chID, p, msgBytes)

   }

   // if err != nil {
   //  // stopServices was invoked and we are shutting down
   //  // receiving is excpected to fail since we will close the connection
   //  select {
   //  case <-c.quitRecvRoutine:
   //    break FOR_LOOP
   //  default:
   //  }

   //  if c.IsRunning() {
   //    if err == io.EOF {
   //      c.Logger.Info("Connection is closed @ recvRoutine (likely by the other side)", "conn", c)
   //    } else {
   //      c.Logger.Error("Connection failed @ recvRoutine (reading byte)", "conn", c, "err", err)
   //    }
   //    c.stopForError(err)
   //  }
   //  break FOR_LOOP
   // }

   // Read more depending on packet type.
   // switch pkt := packet.(type) {
   // case PacketMsg:
   //  channel, ok := c.channelsIdx[pkt.ChannelID]
   //  if !ok || channel == nil {
   //    err := fmt.Errorf("unknown channel %X", pkt.ChannelID)
   //    c.Logger.Error("Connection failed @ recvRoutine", "conn", c, "err", err)
   //    c.stopForError(err)
   //    break FOR_LOOP
   //  }

   //  msgBytes, err := channel.recvPacketMsg(pkt)
   //  if err != nil {
   //    if c.IsRunning() {
   //      c.Logger.Error("Connection failed @ recvRoutine", "conn", c, "err", err)
   //      c.stopForError(err)
   //    }
   //    break FOR_LOOP
   //  }
   //  if msgBytes != nil {
   //    c.Logger.Debug("Received bytes", "chID", pkt.ChannelID, "msgBytes", fmt.Sprintf("%X", msgBytes))
   //    // NOTE: This means the reactor.Receive runs in the same thread as the p2p recv routine
   //    c.onReceive(pkt.ChannelID, msgBytes)
   //  }
   // default:
   //  err := fmt.Errorf("unknown message type %v", reflect.TypeOf(packet))
   //  c.Logger.Error("Connection failed @ recvRoutine", "conn", c, "err", err)
   //  c.stopForError(err)
   //  break FOR_LOOP
   // }
 }
}

// TrySend msg bytes to the channel identified by chID byte. Immediately returns
// false if the send queue is full.
func (p *marlinPeer) TrySend(chID byte, msgBytes []byte) bool {
 return false
}

// Get the data for a given key.
func (p *marlinPeer) Get(key string) interface{} {
 return p.Data.Get(key)
}

// Set sets the data for the given key.
func (p *marlinPeer) Set(key string, data interface{}) {
 p.Data.Set(key, data)
}

// hasChannel returns true if the peer reported
// knowing about the given chID.
func (p *marlinPeer) hasChannel(chID byte) bool {
 for _, ch := range p.channels {
   if ch == chID {
     return true
   }
 }
 // NOTE: probably will want to remove this
 // but could be helpful while the feature is new
 p.Logger.Debug(
   "Unknown channel for peer",
   "channel",
   chID,
   "channels",
   p.channels,
 )
 return false
}

// CloseConn closes original connection. Used for cleaning up in cases where the peer had not been started at all.
func (p *marlinPeer) CloseConn() error {
 return p.peerConn.conn.Close()
}
func (p *marlinPeer) RemoteAddr() net.Addr {
 return p.peerConn.conn.RemoteAddr()
}

var senseMarlinSkips = 500
var currentMarlinSkips = 0
var currentMarlinState = true

// Checks for health and tries to reconnect every once in senseMarlinSkips calls
func (p *marlinPeer) SenseMarlinPortFixIfNeeded() bool {
 if currentMarlinSkips < senseMarlinSkips {
   currentMarlinSkips++
 } else {
   address := p.nodeInfo.(DefaultNodeInfo).ListenAddr
   if currentMarlinState == false {
     currentMarlinState = p.AttemptMarlinTcpReconnect(address)
   }
   port := strings.Split(address, ":")[1]
   p.Logger.Info("Sensing MarlinPort", port)
   currentMarlinSkips = 0
   out, err := exec.Command("lsof","-iTCP:"+port).Output()
   if err == nil && len(out) > 0 {
     currentMarlinState = true
   } else {
     currentMarlinState = false
   }
 }
 return currentMarlinState
}

func (p *marlinPeer) AttemptMarlinTcpReconnect(address string) bool {
 p.Logger.Info("Attempting to Reconnect to MarlinPeer "+address)
 conn, err := net.Dial("tcp", address)
 if err != nil {
   p.Logger.Error("Unable to Reconnect to MarlinPeer "+address, err)
   return false
 }
 p.Logger.Info("Reconnected to MarlinPeer successfully "+address)
 p.bufConnWriter = bufio.NewWriterSize(conn, 65536)
 p.bufConnReader = bufio.NewReaderSize(conn, 1024)
 return true
}
