// Package gateway connects a Sia node to the Sia flood network. The flood
// network is used to propagate blocks and transactions. The gateway is the
// primary avenue that a node uses to hear about transactions and blocks, and
// is the primary avenue used to tell the network about blocks that you have
// mined or about transactions that you have created.
package gateway

// For the user to be securely connected to the network, the user must be
// connected to at least one node which will send them all of the blocks. An
// attacker can trick the user into thinking that a different blockchain is the
// full blockchain if the user is not connected to any nodes who are seeing +
// broadcasting the real chain (and instead is connected only to attacker nodes
// or to nodes that are not broadcasting). This situation is called an eclipse
// attack.
//
// Connecting to a large number of nodes increases the resiliancy of the
// network, but also puts a networking burden on the nodes and can slow down
// block propagation or increase orphan rates. The gateway's job is to keep the
// network efficient while also protecting the user against attacks.
//
// The gateway keeps a list of nodes that it knows about. It uses this list to
// form connections with other nodes, and then uses those connections to
// participate in the flood network. The primary vector for an attacker to
// achieve an eclipse attack is node list domination. If a gateway's nodelist
// is heavily dominated by attacking nodes, then when the gateway chooses to
// make random connections the gateway is at risk of selecting only attacker
// nodes.
//
// The gateway defends itself from these attacks by minimizing the amount of
// control that an attacker has over the node list and peer list. The first
// major defense is that the gateway maintains 8 'outbound' relationships,
// which means that the gateway created those relationships instead of an
// attacker. If a node forms a connection to you, that node is called
// 'inbound', and because it may be an attacker node, it is not trusted.
// Outbound nodes can also be attacker nodes, but they are less likely to be
// attacker nodes because you chose them, instead of them choosing you.
//
// If the gateway forms too many connections, the gateway will allow incoming
// connections by kicking an existing peer. But, to limit the amount of control
// that an attacker may have, only inbound peers are selected to be kicked.
// Furthermore, to increase the difficulty of attack, if a new inbound
// connection shares the same IP address as an existing connection, the shared
// connection is the connection that gets dropped (unless that connection is a
// local or outbound connection).
//
// Nodes are added to a peerlist in two methods. The first method is that a
// gateway will ask its outbound peers for a list of nodes. If the node list is
// below a certain size (see consts.go), the gateway will repeatedly ask
// outbound peers to expand the list. Nodes are also added to the nodelist
// after they successfully form a connection with the gateway. To limit the
// attacker's ability to add nodes to the nodelist, connections are
// ratelimited. An attacker with lots of IP addresses still has the ability to
// fill up the nodelist, however getting 90% dominance of the nodelist requires
// forming thousands of connections, which will take hours or days. By that
// time, the attacked node should already have its set of outbound peers,
// limiting the amount of damage that the attacker can do.
//
// To limit DNS-based tomfoolry, nodes are only added to the nodelist if their
// connection information takes the form of an IP address.
//
// Some research has been done on Bitcoin's flood networks. The more relevant
// research has been listed below. The papers listed first are more relevant.
//     Eclipse Attacks on Bitcoin's Peer-to-Peer Network (Heilman, Kendler, Zohar, Goldberg)
//     Stubborn Mining: Generalizing Selfish Mining and Combining with an Eclipse Attack (Nayak, Kumar, Miller, Shi)
//     An Overview of BGP Hijacking (https://www.bishopfox.com/blog/2015/08/an-overview-of-bgp-hijacking/)

// TODO: Currently the gateway does not do much in terms of bucketing. The
// gateway should make sure that it has outbound peers from a wide range of IP
// addresses, and when kicking inbound peers it shouldn't just favor kicking
// peers of the same IP address, it should favor kicking peers of the same ip
// address range.
//
// TODO: There is no public key exchange, so communications cannot be
// effectively encrypted or authenticated.
//
// TODO: Gateway hostname discovery currently has significant centralization,
// namely the fallback is a single third-party website that can easily form any
// response it wants. Instead, multiple TLS-protected third party websites
// should be used, and the plurality answer should be accepted as the true
// hostname.
//
// TODO: The gateway currently does hostname discovery in a non-blocking way,
// which means that the first few peers that it connects to may not get the
// correct hostname. This means that you may give the remote peer the wrong
// hostname, which means they will not be able to dial you back, which means
// they will not add you to their node list.
//
// TODO: The gateway should encrypt and authenticate all communications. Though
// the gateway participates in a flood network, practical attacks have been
// demonstrated which have been able to confuse nodes by manipulating messages
// from their peers. Encryption + authentication would have made the attack
// more difficult.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/ratelimit"

	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/persist"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"

	siasync "gitlab.com/NebulousLabs/Sia/sync"
)

var errNoPeers = errors.New("no peers")

// Gateway implements the modules.Gateway interface.
type Gateway struct {
	listener net.Listener
	myAddr   modules.NetAddress
	port     string
	rl       *ratelimit.RateLimit

	// handlers are the RPCs that the Gateway can handle.
	//
	// initRPCs are the RPCs that the Gateway calls upon connecting to a peer.
	handlers map[rpcID]modules.RPCFunc
	initRPCs map[string]modules.RPCFunc

	// blacklist are peers that the gateway shouldn't connect to
	//
	// nodes is the set of all known nodes (i.e. potential peers).
	//
	// peers are the nodes that the gateway is currently connected to.
	//
	// peerTG is a special thread group for tracking peer connections, and will
	// block shutdown until all peer connections have been closed out. The peer
	// connections are put in a separate TG because of their unique
	// requirements - they have the potential to live for the lifetime of the
	// program, but also the potential to close early. Calling threads.OnStop
	// for each peer could create a huge backlog of functions that do nothing
	// (because most of the peers disconnected prior to shutdown). And they
	// can't call threads.Add because they are potentially very long running
	// and would block any threads.Flush() calls. So a second threadgroup is
	// added which handles clean-shutdown for the peers, without blocking
	// threads.Flush() calls.
	blacklist map[string]struct{}
	nodes     map[modules.NetAddress]*node
	peers     map[modules.NetAddress]*peer
	peerTG    siasync.ThreadGroup

	// Utilities.
	log           *persist.Logger
	mu            sync.RWMutex
	persist       persistence
	persistDir    string
	threads       siasync.ThreadGroup
	staticAlerter *modules.GenericAlerter
	staticDeps    modules.Dependencies

	// Unique ID
	staticID gatewayID
}

type gatewayID [8]byte

// addToBlacklist adds addresses to the Gateway's blacklist
func (g *Gateway) addToBlacklist(addresses []modules.NetAddress) error {
	// Add addresses to the blacklist and disconnect from them
	var err error
	for _, addr := range addresses {
		p, exists := g.peers[addr]
		if exists {
			err = errors.Compose(err, p.sess.Close())
		}

		// Peer is removed from the peer list as well as the node list, to prevent
		// the node from being re-connected while looking for a replacement peer.
		delete(g.peers, addr)
		delete(g.nodes, addr)
		g.blacklist[addr.Host()] = struct{}{}
	}
	return errors.Compose(err, g.saveSync())
}

// managedSleep will sleep for the given period of time. If the full time
// elapses, 'true' is returned. If the sleep is interrupted for shutdown,
// 'false' is returned.
func (g *Gateway) managedSleep(t time.Duration) (completed bool) {
	select {
	case <-time.After(t):
		return true
	case <-g.threads.StopChan():
		return false
	}
}

// setRateLimits sets the specified ratelimit after performing input
// validation without persisting them.
func setRateLimits(rl *ratelimit.RateLimit, downloadSpeed, uploadSpeed int64) error {
	// Input validation.
	if downloadSpeed < 0 || uploadSpeed < 0 {
		return errors.New("download/upload rate can't be below 0")
	}
	// Check for sentinel "no limits" value.
	if downloadSpeed == 0 && uploadSpeed == 0 {
		rl.SetLimits(0, 0, 0)
	} else {
		rl.SetLimits(downloadSpeed, uploadSpeed, 4*4096)
	}
	return nil
}

// Address returns the NetAddress of the Gateway.
func (g *Gateway) Address() modules.NetAddress {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.myAddr
}

// AddToBlacklist adds addresses to the Gateway's blacklist
func (g *Gateway) AddToBlacklist(addresses []modules.NetAddress) error {
	if err := g.threads.Add(); err != nil {
		return err
	}
	defer g.threads.Done()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addToBlacklist(addresses)
}

// Blacklist returns the Gateway's blacklist
func (g *Gateway) Blacklist() ([]string, error) {
	if err := g.threads.Add(); err != nil {
		return nil, err
	}
	defer g.threads.Done()
	g.mu.RLock()
	defer g.mu.RUnlock()

	var blacklist []string
	for addr := range g.blacklist {
		blacklist = append(blacklist, addr)
	}
	return blacklist, nil
}

// Close saves the state of the Gateway and stops its listener process.
func (g *Gateway) Close() error {
	if err := g.threads.Stop(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return errors.Compose(g.saveSync(), g.saveSyncNodes())
}

// DiscoverAddress discovers and returns the current public IP address of the
// gateway. Contrary to Address, DiscoverAddress is blocking and might take
// multiple minutes to return. A channel to cancel the discovery can be
// supplied optionally. If nil is supplied, a reasonable timeout will be used
// by default.
func (g *Gateway) DiscoverAddress(cancel <-chan struct{}) (net.IP, error) {
	return g.managedLearnHostname(cancel)
}

// ForwardPort adds a port mapping to the router.
func (g *Gateway) ForwardPort(port string) error {
	if err := g.threads.Add(); err != nil {
		return err
	}
	defer g.threads.Done()
	return g.managedForwardPort(port)
}

// RateLimits returns the currently set bandwidth limits of the gateway.
func (g *Gateway) RateLimits() (int64, int64) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.persist.MaxDownloadSpeed, g.persist.MaxUploadSpeed
}

// RemoveFromBlacklist removes addresses from the Gateway's blacklist
func (g *Gateway) RemoveFromBlacklist(addresses []modules.NetAddress) error {
	if err := g.threads.Add(); err != nil {
		return err
	}
	defer g.threads.Done()
	g.mu.Lock()
	defer g.mu.Unlock()

	// Remove addresses from the blacklist
	for _, addr := range addresses {
		delete(g.blacklist, addr.Host())
	}
	return g.saveSync()
}

// SetBlacklist sets the blacklist of the gateway
func (g *Gateway) SetBlacklist(addresses []modules.NetAddress) error {
	if err := g.threads.Add(); err != nil {
		return err
	}
	defer g.threads.Done()
	g.mu.Lock()
	defer g.mu.Unlock()

	// Reset the gateway blacklist since we are replacing the list with the new
	// list of peers
	g.blacklist = make(map[string]struct{})

	// If the length of addresses is 0 we are done, save and return
	if len(addresses) == 0 {
		return g.saveSync()
	}

	// Add addresses to the blacklist and disconnect from them
	return g.addToBlacklist(addresses)
}

// SetRateLimits changes the rate limits for the peer-connections of the
// gateway.
func (g *Gateway) SetRateLimits(downloadSpeed, uploadSpeed int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Set the limit in memory.
	if err := setRateLimits(g.rl, downloadSpeed, uploadSpeed); err != nil {
		return err
	}
	// Update the persistence struct.
	g.persist.MaxDownloadSpeed = downloadSpeed
	g.persist.MaxUploadSpeed = uploadSpeed
	return g.saveSync()
}

// New returns an initialized Gateway.
func New(addr string, bootstrap bool, persistDir string) (*Gateway, error) {
	return NewCustomGateway(addr, bootstrap, persistDir, modules.ProdDependencies)
}

// NewCustomGateway returns an initialized Gateway with custom dependencies.
func NewCustomGateway(addr string, bootstrap bool, persistDir string, deps modules.Dependencies) (*Gateway, error) {
	// Create the directory if it doesn't exist.
	err := os.MkdirAll(persistDir, 0700)
	if err != nil {
		return nil, err
	}

	g := &Gateway{
		handlers: make(map[rpcID]modules.RPCFunc),
		initRPCs: make(map[string]modules.RPCFunc),

		blacklist: make(map[string]struct{}),
		nodes:     make(map[modules.NetAddress]*node),
		peers:     make(map[modules.NetAddress]*peer),

		persistDir:    persistDir,
		staticAlerter: modules.NewAlerter("gateway"),
		staticDeps:    deps,
	}

	// Set Unique GatewayID
	fastrand.Read(g.staticID[:])

	// Create the logger.
	g.log, err = persist.NewFileLogger(filepath.Join(g.persistDir, logFile))
	if err != nil {
		return nil, err
	}
	// Establish the closing of the logger.
	g.threads.AfterStop(func() {
		if err := g.log.Close(); err != nil {
			// The logger may or may not be working here, so use a println
			// instead.
			fmt.Println("Failed to close the gateway logger:", err)
		}
	})
	g.log.Println("INFO: gateway created, started logging")

	// Establish that the peerTG must complete shutdown before the primary
	// thread group completes shutdown.
	g.threads.OnStop(func() {
		err = g.peerTG.Stop()
		if err != nil {
			g.log.Println("ERROR: peerTG experienced errors while shutting down:", err)
		}
	})

	// Register RPCs.
	g.RegisterRPC("ShareNodes", g.shareNodes)
	g.RegisterRPC("DiscoverIP", g.discoverPeerIP)
	g.RegisterConnectCall("ShareNodes", g.requestNodes)
	// Establish the de-registration of the RPCs.
	g.threads.OnStop(func() {
		g.UnregisterRPC("ShareNodes")
		g.UnregisterRPC("DiscoverIP")
		g.UnregisterConnectCall("ShareNodes")
	})

	// Load the old node list and gateway persistence. If it doesn't exist, no
	// problem, but if it does, we want to know about any errors preventing us
	// from loading it.
	if loadErr := g.load(); loadErr != nil && !os.IsNotExist(loadErr) {
		return nil, errors.AddContext(loadErr, "unable to load gateway")
	}
	// Create the ratelimiter and set it to the persisted limits.
	g.rl = ratelimit.NewRateLimit(0, 0, 0)
	if err := setRateLimits(g.rl, g.persist.MaxDownloadSpeed, g.persist.MaxUploadSpeed); err != nil {
		return nil, errors.AddContext(err, "unable to set rate limits for the gateway")
	}
	// Spawn the thread to periodically save the gateway.
	go g.threadedSaveLoop()
	// Make sure that the gateway saves after shutdown.
	g.threads.AfterStop(func() {
		g.mu.Lock()
		if err := g.saveSync(); err != nil {
			g.log.Println("ERROR: Unable to save gateway:", err)
		}
		if err := g.saveSyncNodes(); err != nil {
			g.log.Println("ERROR: Unable to save gateway nodes:", err)
		}
		g.mu.Unlock()
	})

	// Add the bootstrap peers to the node list.
	if bootstrap {
		for _, addr := range modules.BootstrapPeers {
			err := g.addNode(addr)
			if err != nil && err != errNodeExists {
				g.log.Printf("WARN: failed to add the bootstrap node '%v': %v", addr, err)
			}
		}
	}

	// Create the listener which will listen for new connections from peers.
	permanentListenClosedChan := make(chan struct{})
	g.listener, err = net.Listen("tcp", addr)
	if err != nil {
		context := fmt.Sprintf("unable to create gateway tcp listener with address %v", addr)
		return nil, errors.AddContext(err, context)
	}
	// Automatically close the listener when g.threads.Stop() is called.
	g.threads.OnStop(func() {
		err := g.listener.Close()
		if err != nil {
			g.log.Println("WARN: closing the listener failed:", err)
		}
		<-permanentListenClosedChan
	})
	// Set the address and port of the gateway.
	host, port, err := net.SplitHostPort(g.listener.Addr().String())
	g.port = port
	if err != nil {
		context := fmt.Sprintf("unable to split host and port from address %v", g.listener.Addr().String())
		return nil, errors.AddContext(err, context)
	}

	if ip := net.ParseIP(host); ip.IsUnspecified() && ip != nil {
		// if host is unspecified, set a dummy one for now.
		host = "localhost"
	}

	// Set myAddr equal to the address returned by the listener. It will be
	// overwritten by threadedLearnHostname later on.
	g.myAddr = modules.NetAddress(net.JoinHostPort(host, port))

	// Spawn the peer connection listener.
	go g.permanentListen(permanentListenClosedChan)

	// Spawn the peer manager and provide tools for ensuring clean shutdown.
	peerManagerClosedChan := make(chan struct{})
	g.threads.OnStop(func() {
		<-peerManagerClosedChan
	})
	go g.permanentPeerManager(peerManagerClosedChan)

	// Spawn the node manager and provide tools for ensuring clean shutdown.
	nodeManagerClosedChan := make(chan struct{})
	g.threads.OnStop(func() {
		<-nodeManagerClosedChan
	})
	go g.permanentNodeManager(nodeManagerClosedChan)

	// Spawn the node purger and provide tools for ensuring clean shutdown.
	nodePurgerClosedChan := make(chan struct{})
	g.threads.OnStop(func() {
		<-nodePurgerClosedChan
	})
	go g.permanentNodePurger(nodePurgerClosedChan)

	// Spawn threads to take care of port forwarding and hostname discovery.
	go g.threadedForwardPort(g.port)
	go g.threadedLearnHostname()

	// Spawn thread to periodically check if the gateway is online.
	go g.threadedOnlineCheck()

	return g, nil
}

// threadedOnlineCheck periodically calls 'Online' to register the
// GatewayOffline alert.
func (g *Gateway) threadedOnlineCheck() {
	if err := g.threads.Add(); err != nil {
		return
	}
	defer g.threads.Done()
	for {
		select {
		case <-g.threads.StopChan():
			return
		case <-time.After(onlineCheckFrequency):
		}
		_ = g.Online()
	}
}

// enforce that Gateway satisfies the modules.Gateway interface
var _ modules.Gateway = (*Gateway)(nil)
