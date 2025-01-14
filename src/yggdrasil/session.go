package yggdrasil

// This is the session manager
// It's responsible for keeping track of open sessions to other nodes
// The session information consists of crypto keys and coords

import (
	"bytes"
	"container/heap"
	"errors"
	"sync"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/crypto"
	"github.com/yggdrasil-network/yggdrasil-go/src/util"
)

// Duration that we keep track of old nonces per session, to allow some out-of-order packet delivery
const nonceWindow = time.Second

// A heap of nonces, used with a map[nonce]time to allow out-of-order packets a little time to arrive without rejecting them
type nonceHeap []crypto.BoxNonce

func (h nonceHeap) Len() int            { return len(h) }
func (h nonceHeap) Less(i, j int) bool  { return h[i].Minus(&h[j]) < 0 }
func (h nonceHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *nonceHeap) Push(x interface{}) { *h = append(*h, x.(crypto.BoxNonce)) }
func (h *nonceHeap) Pop() interface{} {
	l := len(*h)
	var n crypto.BoxNonce
	n, *h = (*h)[l-1], (*h)[:l-1]
	return n
}
func (h nonceHeap) peek() *crypto.BoxNonce { return &h[len(h)-1] }

// All the information we know about an active session.
// This includes coords, permanent and ephemeral keys, handles and nonces, various sorts of timing information for timeout and maintenance, and some metadata for the admin API.
type sessionInfo struct {
	mutex          sync.Mutex                    // Protects all of the below, use it any time you read/chance the contents of a session
	core           *Core                         //
	reconfigure    chan chan error               //
	theirAddr      address.Address               //
	theirSubnet    address.Subnet                //
	theirPermPub   crypto.BoxPubKey              //
	theirSesPub    crypto.BoxPubKey              //
	mySesPub       crypto.BoxPubKey              //
	mySesPriv      crypto.BoxPrivKey             //
	sharedSesKey   crypto.BoxSharedKey           // derived from session keys
	theirHandle    crypto.Handle                 //
	myHandle       crypto.Handle                 //
	theirNonce     crypto.BoxNonce               //
	theirNonceHeap nonceHeap                     // priority queue to keep track of the lowest nonce we recently accepted
	theirNonceMap  map[crypto.BoxNonce]time.Time // time we added each nonce to the heap
	myNonce        crypto.BoxNonce               //
	theirMTU       uint16                        //
	myMTU          uint16                        //
	wasMTUFixed    bool                          // Was the MTU fixed by a receive error?
	timeOpened     time.Time                     // Time the sessino was opened
	time           time.Time                     // Time we last received a packet
	mtuTime        time.Time                     // time myMTU was last changed
	pingTime       time.Time                     // time the first ping was sent since the last received packet
	pingSend       time.Time                     // time the last ping was sent
	coords         []byte                        // coords of destination
	reset          bool                          // reset if coords change
	tstamp         int64                         // ATOMIC - tstamp from their last session ping, replay attack mitigation
	bytesSent      uint64                        // Bytes of real traffic sent in this session
	bytesRecvd     uint64                        // Bytes of real traffic received in this session
	init           chan struct{}                 // Closed when the first session pong arrives, used to signal that the session is ready for initial use
	cancel         util.Cancellation             // Used to terminate workers
	fromRouter     chan wire_trafficPacket       // Received packets go here, to be decrypted by the session
	recv           chan []byte                   // Decrypted packets go here, picked up by the associated Conn
	send           chan FlowKeyMessage           // Packets with optional flow key go here, to be encrypted and sent
}

func (sinfo *sessionInfo) doFunc(f func()) {
	sinfo.mutex.Lock()
	defer sinfo.mutex.Unlock()
	f()
}

// Represents a session ping/pong packet, andincludes information like public keys, a session handle, coords, a timestamp to prevent replays, and the tun/tap MTU.
type sessionPing struct {
	SendPermPub crypto.BoxPubKey // Sender's permanent key
	Handle      crypto.Handle    // Random number to ID session
	SendSesPub  crypto.BoxPubKey // Session key to use
	Coords      []byte           //
	Tstamp      int64            // unix time, but the only real requirement is that it increases
	IsPong      bool             //
	MTU         uint16           //
}

// Updates session info in response to a ping, after checking that the ping is OK.
// Returns true if the session was updated, or false otherwise.
func (s *sessionInfo) update(p *sessionPing) bool {
	if !(p.Tstamp > s.tstamp) {
		// To protect against replay attacks
		return false
	}
	if p.SendPermPub != s.theirPermPub {
		// Should only happen if two sessions got the same handle
		// That shouldn't be allowed anyway, but if it happens then let one time out
		return false
	}
	if p.SendSesPub != s.theirSesPub {
		s.theirSesPub = p.SendSesPub
		s.theirHandle = p.Handle
		s.sharedSesKey = *crypto.GetSharedKey(&s.mySesPriv, &s.theirSesPub)
		s.theirNonce = crypto.BoxNonce{}
		s.theirNonceHeap = nil
		s.theirNonceMap = make(map[crypto.BoxNonce]time.Time)
	}
	if p.MTU >= 1280 || p.MTU == 0 {
		s.theirMTU = p.MTU
	}
	if !bytes.Equal(s.coords, p.Coords) {
		// allocate enough space for additional coords
		s.coords = append(make([]byte, 0, len(p.Coords)+11), p.Coords...)
	}
	s.time = time.Now()
	s.tstamp = p.Tstamp
	s.reset = false
	defer func() { recover() }() // Recover if the below panics
	select {
	case <-s.init:
	default:
		// Unblock anything waiting for the session to initialize
		close(s.init)
	}
	return true
}

// Struct of all active sessions.
// Sessions are indexed by handle.
// Additionally, stores maps of address/subnet onto keys, and keys onto handles.
type sessions struct {
	core             *Core
	listener         *Listener
	listenerMutex    sync.Mutex
	reconfigure      chan chan error
	lastCleanup      time.Time
	isAllowedHandler func(pubkey *crypto.BoxPubKey, initiator bool) bool // Returns true or false if session setup is allowed
	isAllowedMutex   sync.RWMutex                                        // Protects the above
	permShared       map[crypto.BoxPubKey]*crypto.BoxSharedKey           // Maps known permanent keys to their shared key, used by DHT a lot
	sinfos           map[crypto.Handle]*sessionInfo                      // Maps handle onto session info
	byTheirPerm      map[crypto.BoxPubKey]*crypto.Handle                 // Maps theirPermPub onto handle
}

// Initializes the session struct.
func (ss *sessions) init(core *Core) {
	ss.core = core
	ss.reconfigure = make(chan chan error, 1)
	go func() {
		for {
			e := <-ss.reconfigure
			responses := make(map[crypto.Handle]chan error)
			for index, session := range ss.sinfos {
				responses[index] = make(chan error)
				session.reconfigure <- responses[index]
			}
			for _, response := range responses {
				if err := <-response; err != nil {
					e <- err
					continue
				}
			}
			e <- nil
		}
	}()
	ss.permShared = make(map[crypto.BoxPubKey]*crypto.BoxSharedKey)
	ss.sinfos = make(map[crypto.Handle]*sessionInfo)
	ss.byTheirPerm = make(map[crypto.BoxPubKey]*crypto.Handle)
	ss.lastCleanup = time.Now()
}

// Determines whether the session with a given publickey is allowed based on
// session firewall rules.
func (ss *sessions) isSessionAllowed(pubkey *crypto.BoxPubKey, initiator bool) bool {
	ss.isAllowedMutex.RLock()
	defer ss.isAllowedMutex.RUnlock()

	if ss.isAllowedHandler == nil {
		return true
	}

	return ss.isAllowedHandler(pubkey, initiator)
}

// Gets the session corresponding to a given handle.
func (ss *sessions) getSessionForHandle(handle *crypto.Handle) (*sessionInfo, bool) {
	sinfo, isIn := ss.sinfos[*handle]
	return sinfo, isIn
}

// Gets a session corresponding to a permanent key used by the remote node.
func (ss *sessions) getByTheirPerm(key *crypto.BoxPubKey) (*sessionInfo, bool) {
	h, isIn := ss.byTheirPerm[*key]
	if !isIn {
		return nil, false
	}
	sinfo, isIn := ss.getSessionForHandle(h)
	return sinfo, isIn
}

// Creates a new session and lazily cleans up old existing sessions. This
// includse initializing session info to sane defaults (e.g. lowest supported
// MTU).
func (ss *sessions) createSession(theirPermKey *crypto.BoxPubKey) *sessionInfo {
	// TODO: this check definitely needs to be moved
	if !ss.isSessionAllowed(theirPermKey, true) {
		return nil
	}
	sinfo := sessionInfo{}
	sinfo.core = ss.core
	sinfo.reconfigure = make(chan chan error, 1)
	sinfo.theirPermPub = *theirPermKey
	pub, priv := crypto.NewBoxKeys()
	sinfo.mySesPub = *pub
	sinfo.mySesPriv = *priv
	sinfo.myNonce = *crypto.NewBoxNonce()
	sinfo.theirMTU = 1280
	ss.core.config.Mutex.RLock()
	sinfo.myMTU = uint16(ss.core.config.Current.IfMTU)
	ss.core.config.Mutex.RUnlock()
	now := time.Now()
	sinfo.timeOpened = now
	sinfo.time = now
	sinfo.mtuTime = now
	sinfo.pingTime = now
	sinfo.pingSend = now
	sinfo.init = make(chan struct{})
	sinfo.cancel = util.NewCancellation()
	higher := false
	for idx := range ss.core.boxPub {
		if ss.core.boxPub[idx] > sinfo.theirPermPub[idx] {
			higher = true
			break
		} else if ss.core.boxPub[idx] < sinfo.theirPermPub[idx] {
			break
		}
	}
	if higher {
		// higher => odd nonce
		sinfo.myNonce[len(sinfo.myNonce)-1] |= 0x01
	} else {
		// lower => even nonce
		sinfo.myNonce[len(sinfo.myNonce)-1] &= 0xfe
	}
	sinfo.myHandle = *crypto.NewHandle()
	sinfo.theirAddr = *address.AddrForNodeID(crypto.GetNodeID(&sinfo.theirPermPub))
	sinfo.theirSubnet = *address.SubnetForNodeID(crypto.GetNodeID(&sinfo.theirPermPub))
	sinfo.fromRouter = make(chan wire_trafficPacket, 1)
	sinfo.recv = make(chan []byte, 32)
	sinfo.send = make(chan FlowKeyMessage, 32)
	ss.sinfos[sinfo.myHandle] = &sinfo
	ss.byTheirPerm[sinfo.theirPermPub] = &sinfo.myHandle
	go func() {
		// Run cleanup when the session is canceled
		<-sinfo.cancel.Finished()
		sinfo.core.router.doAdmin(sinfo.close)
	}()
	go sinfo.startWorkers()
	return &sinfo
}

func (ss *sessions) cleanup() {
	// Time thresholds almost certainly could use some adjusting
	for k := range ss.permShared {
		// Delete a key, to make sure this eventually shrinks to 0
		delete(ss.permShared, k)
		break
	}
	if time.Since(ss.lastCleanup) < time.Minute {
		return
	}
	permShared := make(map[crypto.BoxPubKey]*crypto.BoxSharedKey, len(ss.permShared))
	for k, v := range ss.permShared {
		permShared[k] = v
	}
	ss.permShared = permShared
	sinfos := make(map[crypto.Handle]*sessionInfo, len(ss.sinfos))
	for k, v := range ss.sinfos {
		sinfos[k] = v
	}
	ss.sinfos = sinfos
	byTheirPerm := make(map[crypto.BoxPubKey]*crypto.Handle, len(ss.byTheirPerm))
	for k, v := range ss.byTheirPerm {
		byTheirPerm[k] = v
	}
	ss.byTheirPerm = byTheirPerm
	ss.lastCleanup = time.Now()
}

// Closes a session, removing it from sessions maps.
func (sinfo *sessionInfo) close() {
	if s := sinfo.core.sessions.sinfos[sinfo.myHandle]; s == sinfo {
		delete(sinfo.core.sessions.sinfos, sinfo.myHandle)
		delete(sinfo.core.sessions.byTheirPerm, sinfo.theirPermPub)
	}
}

// Returns a session ping appropriate for the given session info.
func (ss *sessions) getPing(sinfo *sessionInfo) sessionPing {
	loc := ss.core.switchTable.getLocator()
	coords := loc.getCoords()
	ref := sessionPing{
		SendPermPub: ss.core.boxPub,
		Handle:      sinfo.myHandle,
		SendSesPub:  sinfo.mySesPub,
		Tstamp:      time.Now().Unix(),
		Coords:      coords,
		MTU:         sinfo.myMTU,
	}
	sinfo.myNonce.Increment()
	return ref
}

// Gets the shared key for a pair of box keys.
// Used to cache recently used shared keys for protocol traffic.
// This comes up with dht req/res and session ping/pong traffic.
func (ss *sessions) getSharedKey(myPriv *crypto.BoxPrivKey,
	theirPub *crypto.BoxPubKey) *crypto.BoxSharedKey {
	return crypto.GetSharedKey(myPriv, theirPub)
	// FIXME concurrency issues with the below, so for now we just burn the CPU every time
	if skey, isIn := ss.permShared[*theirPub]; isIn {
		return skey
	}
	// First do some cleanup
	const maxKeys = 1024
	for key := range ss.permShared {
		// Remove a random key until the store is small enough
		if len(ss.permShared) < maxKeys {
			break
		}
		delete(ss.permShared, key)
	}
	ss.permShared[*theirPub] = crypto.GetSharedKey(myPriv, theirPub)
	return ss.permShared[*theirPub]
}

// Sends a session ping by calling sendPingPong in ping mode.
func (ss *sessions) ping(sinfo *sessionInfo) {
	ss.sendPingPong(sinfo, false)
}

// Calls getPing, sets the appropriate ping/pong flag, encodes to wire format, and send it.
// Updates the time the last ping was sent in the session info.
func (ss *sessions) sendPingPong(sinfo *sessionInfo, isPong bool) {
	ping := ss.getPing(sinfo)
	ping.IsPong = isPong
	bs := ping.encode()
	shared := ss.getSharedKey(&ss.core.boxPriv, &sinfo.theirPermPub)
	payload, nonce := crypto.BoxSeal(shared, bs, nil)
	p := wire_protoTrafficPacket{
		Coords:  sinfo.coords,
		ToKey:   sinfo.theirPermPub,
		FromKey: ss.core.boxPub,
		Nonce:   *nonce,
		Payload: payload,
	}
	packet := p.encode()
	ss.core.router.out(packet)
	if sinfo.pingTime.Before(sinfo.time) {
		sinfo.pingTime = time.Now()
	}
}

// Handles a session ping, creating a session if needed and calling update, then possibly responding with a pong if the ping was in ping mode and the update was successful.
// If the session has a packet cached (common when first setting up a session), it will be sent.
func (ss *sessions) handlePing(ping *sessionPing) {
	// Get the corresponding session (or create a new session)
	sinfo, isIn := ss.getByTheirPerm(&ping.SendPermPub)
	switch {
	case isIn: // Session already exists
	case !ss.isSessionAllowed(&ping.SendPermPub, false): // Session is not allowed
	case ping.IsPong: // This is a response, not an initial ping, so ignore it.
	default:
		ss.listenerMutex.Lock()
		if ss.listener != nil {
			// This is a ping from an allowed node for which no session exists, and we have a listener ready to handle sessions.
			// We need to create a session and pass it to the listener.
			sinfo = ss.createSession(&ping.SendPermPub)
			if s, _ := ss.getByTheirPerm(&ping.SendPermPub); s != sinfo {
				panic("This should not happen")
			}
			conn := newConn(ss.core, crypto.GetNodeID(&sinfo.theirPermPub), &crypto.NodeID{}, sinfo)
			for i := range conn.nodeMask {
				conn.nodeMask[i] = 0xFF
			}
			c := ss.listener.conn
			go func() { c <- conn }()
		}
		ss.listenerMutex.Unlock()
	}
	if sinfo != nil {
		sinfo.doFunc(func() {
			// Update the session
			if !sinfo.update(ping) { /*panic("Should not happen in testing")*/
				return
			}
			if !ping.IsPong {
				ss.sendPingPong(sinfo, true)
			}
		})
	}
}

// Get the MTU of the session.
// Will be equal to the smaller of this node's MTU or the remote node's MTU.
// If sending over links with a maximum message size (this was a thing with the old UDP code), it could be further lowered, to a minimum of 1280.
func (sinfo *sessionInfo) getMTU() uint16 {
	if sinfo.theirMTU == 0 || sinfo.myMTU == 0 {
		return 0
	}
	if sinfo.theirMTU < sinfo.myMTU {
		return sinfo.theirMTU
	}
	return sinfo.myMTU
}

// Checks if a packet's nonce is recent enough to fall within the window of allowed packets, and not already received.
func (sinfo *sessionInfo) nonceIsOK(theirNonce *crypto.BoxNonce) bool {
	// The bitmask is to allow for some non-duplicate out-of-order packets
	if theirNonce.Minus(&sinfo.theirNonce) > 0 {
		// This is newer than the newest nonce we've seen
		return true
	}
	if len(sinfo.theirNonceHeap) > 0 {
		if theirNonce.Minus(sinfo.theirNonceHeap.peek()) > 0 {
			if _, isIn := sinfo.theirNonceMap[*theirNonce]; !isIn {
				// This nonce is recent enough that we keep track of older nonces, but it's not one we've seen yet
				return true
			}
		}
	}
	return false
}

// Updates the nonce mask by (possibly) shifting the bitmask and setting the bit corresponding to this nonce to 1, and then updating the most recent nonce
func (sinfo *sessionInfo) updateNonce(theirNonce *crypto.BoxNonce) {
	// Start with some cleanup
	for len(sinfo.theirNonceHeap) > 64 {
		if time.Since(sinfo.theirNonceMap[*sinfo.theirNonceHeap.peek()]) < nonceWindow {
			// This nonce is still fairly new, so keep it around
			break
		}
		// TODO? reallocate the map in some cases, to free unused map space?
		delete(sinfo.theirNonceMap, *sinfo.theirNonceHeap.peek())
		heap.Pop(&sinfo.theirNonceHeap)
	}
	if theirNonce.Minus(&sinfo.theirNonce) > 0 {
		// This nonce is the newest we've seen, so make a note of that
		sinfo.theirNonce = *theirNonce
	}
	// Add it to the heap/map so we know not to allow it again
	heap.Push(&sinfo.theirNonceHeap, *theirNonce)
	sinfo.theirNonceMap[*theirNonce] = time.Now()
}

// Resets all sessions to an uninitialized state.
// Called after coord changes, so attemtps to use a session will trigger a new ping and notify the remote end of the coord change.
func (ss *sessions) reset() {
	for _, sinfo := range ss.sinfos {
		sinfo.doFunc(func() {
			sinfo.reset = true
		})
	}
}

////////////////////////////////////////////////////////////////////////////////
//////////////////////////// Worker Functions Below ////////////////////////////
////////////////////////////////////////////////////////////////////////////////

func (sinfo *sessionInfo) startWorkers() {
	go sinfo.recvWorker()
	go sinfo.sendWorker()
}

type FlowKeyMessage struct {
	FlowKey uint64
	Message []byte
}

func (sinfo *sessionInfo) recvWorker() {
	// TODO move theirNonce etc into a struct that gets stored here, passed in over a channel
	//  Since there's no reason for anywhere else in the session code to need to *read* it...
	//  Only needs to be updated from the outside if a ping resets it...
	//  That would get rid of the need to take a mutex for the sessionFunc
	var callbacks []chan func()
	doRecv := func(p wire_trafficPacket) {
		var bs []byte
		var err error
		var k crypto.BoxSharedKey
		sessionFunc := func() {
			if !sinfo.nonceIsOK(&p.Nonce) {
				err = ConnError{errors.New("packet dropped due to invalid nonce"), false, true, false, 0}
				return
			}
			k = sinfo.sharedSesKey
		}
		sinfo.doFunc(sessionFunc)
		if err != nil {
			util.PutBytes(p.Payload)
			return
		}
		var isOK bool
		ch := make(chan func(), 1)
		poolFunc := func() {
			bs, isOK = crypto.BoxOpen(&k, p.Payload, &p.Nonce)
			callback := func() {
				util.PutBytes(p.Payload)
				if !isOK {
					util.PutBytes(bs)
					return
				}
				sessionFunc = func() {
					if k != sinfo.sharedSesKey || !sinfo.nonceIsOK(&p.Nonce) {
						// The session updated in the mean time, so return an error
						err = ConnError{errors.New("session updated during crypto operation"), false, true, false, 0}
						return
					}
					sinfo.updateNonce(&p.Nonce)
					sinfo.time = time.Now()
					sinfo.bytesRecvd += uint64(len(bs))
				}
				sinfo.doFunc(sessionFunc)
				if err != nil {
					// Not sure what else to do with this packet, I guess just drop it
					util.PutBytes(bs)
				} else {
					// Pass the packet to the buffer for Conn.Read
					select {
					case <-sinfo.cancel.Finished():
						util.PutBytes(bs)
					case sinfo.recv <- bs:
					}
				}
			}
			ch <- callback
		}
		// Send to the worker and wait for it to finish
		util.WorkerGo(poolFunc)
		callbacks = append(callbacks, ch)
	}
	fromHelper := make(chan wire_trafficPacket, 1)
	go func() {
		var buf []wire_trafficPacket
		for {
			for len(buf) > 0 {
				select {
				case <-sinfo.cancel.Finished():
					return
				case p := <-sinfo.fromRouter:
					buf = append(buf, p)
					for len(buf) > 64 { // Based on nonce window size
						util.PutBytes(buf[0].Payload)
						buf = buf[1:]
					}
				case fromHelper <- buf[0]:
					buf = buf[1:]
				}
			}
			select {
			case <-sinfo.cancel.Finished():
				return
			case p := <-sinfo.fromRouter:
				buf = append(buf, p)
			}
		}
	}()
	select {
	case <-sinfo.cancel.Finished():
		return
	case <-sinfo.init:
		// Wait until the session has finished initializing before processing any packets
	}
	for {
		for len(callbacks) > 0 {
			select {
			case f := <-callbacks[0]:
				callbacks = callbacks[1:]
				f()
			case <-sinfo.cancel.Finished():
				return
			case p := <-fromHelper:
				doRecv(p)
			}
		}
		select {
		case <-sinfo.cancel.Finished():
			return
		case p := <-fromHelper:
			doRecv(p)
		}
	}
}

func (sinfo *sessionInfo) sendWorker() {
	// TODO move info that this worker needs here, send updates via a channel
	//  Otherwise we need to take a mutex to avoid races with update()
	var callbacks []chan func()
	doSend := func(msg FlowKeyMessage) {
		var p wire_trafficPacket
		var k crypto.BoxSharedKey
		sessionFunc := func() {
			sinfo.bytesSent += uint64(len(msg.Message))
			p = wire_trafficPacket{
				Coords: append([]byte(nil), sinfo.coords...),
				Handle: sinfo.theirHandle,
				Nonce:  sinfo.myNonce,
			}
			if msg.FlowKey != 0 {
				// Helps ensure that traffic from this flow ends up in a separate queue from other flows
				// The zero padding relies on the fact that the self-peer is always on port 0
				p.Coords = append(p.Coords, 0)
				p.Coords = wire_put_uint64(msg.FlowKey, p.Coords)
			}
			sinfo.myNonce.Increment()
			k = sinfo.sharedSesKey
		}
		// Get the mutex-protected info needed to encrypt the packet
		sinfo.doFunc(sessionFunc)
		ch := make(chan func(), 1)
		poolFunc := func() {
			// Encrypt the packet
			p.Payload, _ = crypto.BoxSeal(&k, msg.Message, &p.Nonce)
			// The callback will send the packet
			callback := func() {
				// Encoding may block on a util.GetBytes(), so kept out of the worker pool
				packet := p.encode()
				// Cleanup
				util.PutBytes(msg.Message)
				util.PutBytes(p.Payload)
				// Send the packet
				sinfo.core.router.out(packet)
			}
			ch <- callback
		}
		// Send to the worker and wait for it to finish
		util.WorkerGo(poolFunc)
		callbacks = append(callbacks, ch)
	}
	select {
	case <-sinfo.cancel.Finished():
		return
	case <-sinfo.init:
		// Wait until the session has finished initializing before processing any packets
	}
	for {
		for len(callbacks) > 0 {
			select {
			case f := <-callbacks[0]:
				callbacks = callbacks[1:]
				f()
			case <-sinfo.cancel.Finished():
				return
			case msg := <-sinfo.send:
				doSend(msg)
			}
		}
		select {
		case <-sinfo.cancel.Finished():
			return
		case bs := <-sinfo.send:
			doSend(bs)
		}
	}
}
