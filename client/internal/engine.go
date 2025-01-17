package internal

import (
	"fmt"
	"github.com/cenkalti/backoff/v4"
	ice "github.com/pion/ice/v2"
	log "github.com/sirupsen/logrus"
	"github.com/wiretrustee/wiretrustee/iface"
	mgm "github.com/wiretrustee/wiretrustee/management/client"
	mgmProto "github.com/wiretrustee/wiretrustee/management/proto"
	signal "github.com/wiretrustee/wiretrustee/signal/client"
	sProto "github.com/wiretrustee/wiretrustee/signal/proto"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"strings"
	"sync"
	"time"
)

// PeerConnectionTimeout is a timeout of an initial connection attempt to a remote peer.
// E.g. this peer will wait PeerConnectionTimeout for the remote peer to respond, if not successful then it will retry the connection attempt.
const PeerConnectionTimeout = 60 * time.Second

// EngineConfig is a config for the Engine
type EngineConfig struct {
	// StunsTurns is a list of STUN and TURN servers used by ICE
	StunsTurns []*ice.URL
	WgIface    string
	// WgAddr is a Wireguard local address (Wiretrustee Network IP)
	WgAddr string
	// WgPrivateKey is a Wireguard private key of our peer (it MUST never leave the machine)
	WgPrivateKey wgtypes.Key
	// IFaceBlackList is a list of network interfaces to ignore when discovering connection candidates (ICE related)
	IFaceBlackList map[string]struct{}
}

// Engine is a mechanism responsible for reacting on Signal and Management stream events and managing connections to the remote peers.
type Engine struct {
	// signal is a Signal Service client
	signal *signal.Client
	// mgmClient is a Management Service client
	mgmClient *mgm.Client
	// conns is a collection of remote peer connections indexed by local public key of the remote peers
	conns map[string]*Connection

	// peerMux is used to sync peer operations (e.g. open connection, peer removal)
	peerMux *sync.Mutex
	// syncMsgMux is used to guarantee sequential Management Service message processing
	syncMsgMux *sync.Mutex

	config *EngineConfig

	// wgPort is a Wireguard local listen port
	wgPort int
}

// Peer is an instance of the Connection Peer
type Peer struct {
	WgPubKey     string
	WgAllowedIps string
}

// NewEngine creates a new Connection Engine
func NewEngine(signalClient *signal.Client, mgmClient *mgm.Client, config *EngineConfig) *Engine {
	return &Engine{
		signal:     signalClient,
		mgmClient:  mgmClient,
		conns:      map[string]*Connection{},
		peerMux:    &sync.Mutex{},
		syncMsgMux: &sync.Mutex{},
		config:     config,
	}
}

// Start creates a new Wireguard tunnel interface and listens to events from Signal and Management services
// Connections to remote peers are not established here.
// However, they will be established once an event with a list of peers to connect to will be received from Management Service
func (e *Engine) Start() error {

	wgIface := e.config.WgIface
	wgAddr := e.config.WgAddr
	myPrivateKey := e.config.WgPrivateKey

	err := iface.Create(wgIface, wgAddr)
	if err != nil {
		log.Errorf("failed creating interface %s: [%s]", wgIface, err.Error())
		return err
	}

	err = iface.Configure(wgIface, myPrivateKey.String())
	if err != nil {
		log.Errorf("failed configuring Wireguard interface [%s]: %s", wgIface, err.Error())
		return err
	}

	port, err := iface.GetListenPort(wgIface)
	if err != nil {
		log.Errorf("failed getting Wireguard listen port [%s]: %s", wgIface, err.Error())
		return err
	}
	e.wgPort = *port

	e.receiveSignalEvents()
	e.receiveManagementEvents()

	return nil
}

// initializePeer peer agent attempt to open connection
func (e *Engine) initializePeer(peer Peer) {
	var backOff = &backoff.ExponentialBackOff{
		InitialInterval:     backoff.DefaultInitialInterval,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          backoff.DefaultMultiplier,
		MaxInterval:         5 * time.Second,
		MaxElapsedTime:      time.Duration(0), //never stop
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}
	operation := func() error {
		_, err := e.openPeerConnection(e.wgPort, e.config.WgPrivateKey, peer)
		e.peerMux.Lock()
		defer e.peerMux.Unlock()
		if _, ok := e.conns[peer.WgPubKey]; !ok {
			log.Infof("removing connection attempt with Peer: %v, not retrying", peer.WgPubKey)
			return nil
		}

		if err != nil {
			log.Warnln(err)
			log.Warnln("retrying connection because of error: ", err.Error())
			return err
		}
		return nil
	}

	err := backoff.Retry(operation, backOff)
	if err != nil {
		// should actually never happen
		panic(err)
	}
}

func (e *Engine) removePeerConnections(peers []string) error {
	e.peerMux.Lock()
	defer e.peerMux.Unlock()
	for _, peer := range peers {
		err := e.removePeerConnection(peer)
		if err != nil {
			return err
		}
	}
	return nil
}

// removePeerConnection closes existing peer connection and removes peer
func (e *Engine) removePeerConnection(peerKey string) error {
	conn, exists := e.conns[peerKey]
	if exists && conn != nil {
		delete(e.conns, peerKey)
		return conn.Close()
	}
	return nil
}

// GetPeerConnectionStatus returns a connection Status or nil if peer connection wasn't found
func (e *Engine) GetPeerConnectionStatus(peerKey string) *Status {
	e.peerMux.Lock()
	defer e.peerMux.Unlock()

	conn, exists := e.conns[peerKey]
	if exists && conn != nil {
		return &conn.Status
	}

	return nil
}

// openPeerConnection opens a new remote peer connection
func (e *Engine) openPeerConnection(wgPort int, myKey wgtypes.Key, peer Peer) (*Connection, error) {
	e.peerMux.Lock()

	remoteKey, _ := wgtypes.ParseKey(peer.WgPubKey)
	connConfig := &ConnConfig{
		WgListenAddr:   fmt.Sprintf("127.0.0.1:%d", wgPort),
		WgPeerIP:       e.config.WgAddr,
		WgIface:        e.config.WgIface,
		WgAllowedIPs:   peer.WgAllowedIps,
		WgKey:          myKey,
		RemoteWgKey:    remoteKey,
		StunTurnURLS:   e.config.StunsTurns,
		iFaceBlackList: e.config.IFaceBlackList,
	}

	signalOffer := func(uFrag string, pwd string) error {
		return signalAuth(uFrag, pwd, myKey, remoteKey, e.signal, false)
	}

	signalAnswer := func(uFrag string, pwd string) error {
		return signalAuth(uFrag, pwd, myKey, remoteKey, e.signal, true)
	}
	signalCandidate := func(candidate ice.Candidate) error {
		return signalCandidate(candidate, myKey, remoteKey, e.signal)
	}
	conn := NewConnection(*connConfig, signalCandidate, signalOffer, signalAnswer)
	e.conns[remoteKey.String()] = conn
	e.peerMux.Unlock()

	// blocks until the connection is open (or timeout)
	err := conn.Open(PeerConnectionTimeout)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func signalCandidate(candidate ice.Candidate, myKey wgtypes.Key, remoteKey wgtypes.Key, s *signal.Client) error {
	err := s.Send(&sProto.Message{
		Key:       myKey.PublicKey().String(),
		RemoteKey: remoteKey.String(),
		Body: &sProto.Body{
			Type:    sProto.Body_CANDIDATE,
			Payload: candidate.Marshal(),
		},
	})
	if err != nil {
		log.Errorf("failed signaling candidate to the remote peer %s %s", remoteKey.String(), err)
		//todo ??
		return err
	}

	return nil
}

func signalAuth(uFrag string, pwd string, myKey wgtypes.Key, remoteKey wgtypes.Key, s *signal.Client, isAnswer bool) error {

	var t sProto.Body_Type
	if isAnswer {
		t = sProto.Body_ANSWER
	} else {
		t = sProto.Body_OFFER
	}

	msg, err := signal.MarshalCredential(myKey, remoteKey, &signal.Credential{
		UFrag: uFrag,
		Pwd:   pwd}, t)
	if err != nil {
		return err
	}
	err = s.Send(msg)
	if err != nil {
		return err
	}

	return nil
}

// receiveManagementEvents connects to the Management Service event stream to receive updates from the management service
// E.g. when a new peer has been registered and we are allowed to connect to it.
func (e *Engine) receiveManagementEvents() {

	log.Debugf("connecting to Management Service updates stream")

	e.mgmClient.Sync(func(update *mgmProto.SyncResponse) error {
		// todo handle changes of global settings (in update.GetWiretrusteeConfig())
		// todo handle changes of peer settings (in update.GetPeerConfig())

		e.syncMsgMux.Lock()
		defer e.syncMsgMux.Unlock()

		remotePeers := update.GetRemotePeers()
		if len(remotePeers) != 0 {

			remotePeerMap := make(map[string]struct{})
			for _, peer := range remotePeers {
				remotePeerMap[peer.GetWgPubKey()] = struct{}{}
			}

			//remove peers that are no longer available for us
			toRemove := []string{}
			for p := range e.conns {
				if _, ok := remotePeerMap[p]; !ok {
					toRemove = append(toRemove, p)
				}
			}
			err := e.removePeerConnections(toRemove)
			if err != nil {
				return err
			}

			// add new peers
			for _, peer := range remotePeers {
				peerKey := peer.GetWgPubKey()
				peerIPs := peer.GetAllowedIps()
				if _, ok := e.conns[peerKey]; !ok {
					go e.initializePeer(Peer{
						WgPubKey:     peerKey,
						WgAllowedIps: strings.Join(peerIPs, ","),
					})
				}

			}
		}

		return nil
	})

	log.Infof("connected to Management Service updates stream")
}

// receiveSignalEvents connects to the Signal Service event stream to negotiate connection with remote peers
func (e *Engine) receiveSignalEvents() {
	// connect to a stream of messages coming from the signal server
	e.signal.Receive(func(msg *sProto.Message) error {

		e.syncMsgMux.Lock()
		defer e.syncMsgMux.Unlock()

		conn := e.conns[msg.Key]
		if conn == nil {
			return fmt.Errorf("wrongly addressed message %s", msg.Key)
		}

		if conn.Config.RemoteWgKey.String() != msg.Key {
			return fmt.Errorf("unknown peer %s", msg.Key)
		}

		switch msg.GetBody().Type {
		case sProto.Body_OFFER:
			remoteCred, err := signal.UnMarshalCredential(msg)
			if err != nil {
				return err
			}
			err = conn.OnOffer(IceCredentials{
				uFrag: remoteCred.UFrag,
				pwd:   remoteCred.Pwd,
			})

			if err != nil {
				return err
			}

			return nil
		case sProto.Body_ANSWER:
			remoteCred, err := signal.UnMarshalCredential(msg)
			if err != nil {
				return err
			}
			err = conn.OnAnswer(IceCredentials{
				uFrag: remoteCred.UFrag,
				pwd:   remoteCred.Pwd,
			})

			if err != nil {
				return err
			}

		case sProto.Body_CANDIDATE:

			candidate, err := ice.UnmarshalCandidate(msg.GetBody().Payload)
			if err != nil {
				log.Errorf("failed on parsing remote candidate %s -> %s", candidate, err)
				return err
			}

			err = conn.OnRemoteCandidate(candidate)
			if err != nil {
				log.Errorf("error handling CANDIATE from %s", msg.Key)
				return err
			}
		}

		return nil
	})

	e.signal.WaitConnected()
}
