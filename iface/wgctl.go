package iface

import (
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"net"
	"time"
)

// Extends the functionality of Configure(iface string, privateKey string) by generating a new Wireguard private key
func ConfigureWithKeyGen(iface string) (*wgtypes.Key, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &key, Configure(iface, key.String())
}

// Configures a Wireguard interface
// The interface must exist before calling this method (e.g. call interface.Create() before)
func Configure(iface string, privateKey string) error {

	log.Debugf("configuring Wireguard interface %s", iface)
	wg, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer wg.Close()

	log.Debugf("adding Wireguard private key")
	key, err := wgtypes.ParseKey(privateKey)
	if err != nil {
		return err
	}
	fwmark := 0
	cfg := wgtypes.Config{
		PrivateKey:   &key,
		ReplacePeers: false,
		FirewallMark: &fwmark,
	}
	err = wg.ConfigureDevice(iface, cfg)
	if err != nil {
		return err
	}

	return nil
}

func GetListenPort(iface string) (*int, error) {
	log.Debugf("getting Wireguard listen port of interface %s", iface)

	//discover Wireguard current configuration
	wg, err := wgctrl.New()
	if err != nil {
		return nil, err
	}
	defer wg.Close()

	d, err := wg.Device(iface)
	if err != nil {
		return nil, err
	}
	log.Debugf("got Wireguard device listen port %s, %d", iface, &d.ListenPort)

	return &d.ListenPort, nil
}

// Updates a Wireguard interface listen port
func UpdateListenPort(iface string, newPort int) error {
	log.Debugf("updating Wireguard listen port of interface %s, new port %d", iface, newPort)

	//discover Wireguard current configuration
	wg, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer wg.Close()

	_, err = wg.Device(iface)
	if err != nil {
		return err
	}
	log.Debugf("got Wireguard device %s", iface)

	config := wgtypes.Config{
		ListenPort:   &newPort,
		ReplacePeers: false,
	}
	err = wg.ConfigureDevice(iface, config)
	if err != nil {
		return err
	}

	log.Debugf("updated Wireguard listen port of interface %s, new port %d", iface, newPort)

	return nil
}

func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, []byte(n+"\x00"))
	return b
}

// Updates existing Wireguard Peer or creates a new one if doesn't exist
// Endpoint is optional
func UpdatePeer(iface string, peerKey string, allowedIps string, keepAlive time.Duration, endpoint string) error {
	wg, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer wg.Close()

	_, err = wg.Device(iface)
	if err != nil {
		return err
	}
	log.Debugf("got Wireguard device %s", iface)

	//parse allowed ips
	ipNet, err := netlink.ParseIPNet(allowedIps)
	if err != nil {
		return err
	}

	peerKeyParsed, err := wgtypes.ParseKey(peerKey)

	peers := make([]wgtypes.PeerConfig, 0)
	peer := wgtypes.PeerConfig{
		PublicKey:                   peerKeyParsed,
		ReplaceAllowedIPs:           true,
		AllowedIPs:                  []net.IPNet{*ipNet},
		PersistentKeepaliveInterval: &keepAlive,
	}
	peers = append(peers, peer)

	config := wgtypes.Config{
		ReplacePeers: false,
		Peers:        peers,
	}
	err = wg.ConfigureDevice(iface, config)
	if err != nil {
		return err
	}

	if endpoint != "" {
		return UpdatePeerEndpoint(iface, peerKey, endpoint)
	}

	return nil
}

// Updates a Wireguard interface Peer with the new endpoint
// Used when NAT hole punching was successful and an update of the remote peer endpoint is required
func UpdatePeerEndpoint(iface string, peerKey string, newEndpoint string) error {

	wg, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer wg.Close()

	_, err = wg.Device(iface)
	if err != nil {
		return err
	}
	log.Debugf("got Wireguard device %s", iface)

	peerAddr, err := net.ResolveUDPAddr("udp4", newEndpoint)
	if err != nil {
		return err
	}

	log.Debugf("parsed peer endpoint [%s]", peerAddr.String())

	peerKeyParsed, err := wgtypes.ParseKey(peerKey)
	peers := make([]wgtypes.PeerConfig, 0)
	peer := wgtypes.PeerConfig{
		PublicKey:         peerKeyParsed,
		ReplaceAllowedIPs: false,
		UpdateOnly:        true,
		Endpoint:          peerAddr,
	}
	peers = append(peers, peer)

	config := wgtypes.Config{
		ReplacePeers: false,
		Peers:        peers,
	}
	err = wg.ConfigureDevice(iface, config)
	if err != nil {
		return err
	}

	return nil
}

type wgLink struct {
	attrs *netlink.LinkAttrs
}

func (w *wgLink) Attrs() *netlink.LinkAttrs {
	return w.attrs
}

func (w *wgLink) Type() string {
	return "wireguard"
}