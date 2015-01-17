package synapse

import (
	"errors"
	"github.com/tinylib/msgp/msgp"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

var (
	ErrNoClients = errors.New("synapse: no clients available")
)

// DialCluster creates a client out of multiple servers. It tries
// to maintain connections to the servers by re-dialing them on
// connection failures and removing problematic nodes from the
// connection pool. You must supply at least one remote address.
func DialCluster(network string, addrs ...string) (*ClusterClient, error) {
	if len(addrs) < 1 {
		return nil, ErrNoClients
	}

	c := &ClusterClient{
		state:   2,
		nwk:     network,
		remotes: addrs,
		done:    make(chan struct{}),
	}
	err := c.dialAll()
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// dialAll dials all of the known
// connections *if* there are zero
// clients in the current list. it
// holds the client list lock for
// the entirety of the dialing period.
func (c *ClusterClient) dialAll() error {
	c.cllock.Lock()

	// we could have raced
	// on acquiring the lock,
	// but we only want one process
	// trying to re-dial connections
	if len(c.clients) > 0 {
		c.cllock.Unlock()
		return nil
	}

	// temporary lock for growing
	// the client list
	addLock := new(sync.Mutex)

	// lock access to remotes
	c.remoteLock.Lock()

	if len(c.remotes) == 0 {
		c.remoteLock.Unlock()
		c.cllock.Unlock()
		return ErrNoClients
	}

	errs := make([]error, len(c.remotes))
	wg := new(sync.WaitGroup)
	wg.Add(len(c.remotes))

	for i, addr := range c.remotes {
		go func(idx int, addr string, wg *sync.WaitGroup) {
			defer wg.Done()
			conn, err := net.Dial(c.nwk, addr)
			if err != nil {
				log.Printf("synapse cluster: error dialing %s %s: %s", c.nwk, addr, err)
				c.handleDialError(addr, err)
				errs[idx] = err
				return
			}

			// we can take advantage of resolved DNS, etc.
			// if we don't do this, then Stats() will break,
			// b/c we compare RemoteAddr(), which is not
			// necessarily the input address
			c.remotes[idx] = conn.RemoteAddr().String()

			cl, err := NewClient(conn, 1000)
			if err != nil {
				log.Printf("synapse cluster: error establishing sync with %s %s: %s", c.nwk, c.remotes[idx], err)
				errs[idx] = err
				return
			}
			addLock.Lock()
			c.clients = append(c.clients, cl)
			addLock.Unlock()
			return
		}(i, addr, wg)
	}

	wg.Wait()
	c.remoteLock.Unlock()
	c.cllock.Unlock()

	// we only return an
	// error if we are unable
	// to reach *any* of the
	// servers
	any := false
	first := -1
	for i, err := range errs {
		if err == nil && !any {
			any = true
		} else if first == -1 && err != nil {
			first = i
		}
	}
	if !any {
		return errs[first]
	}
	return nil
}

// ClusterClient represents a pool of
// connections to one or more servers.
type ClusterClient struct {
	idx        uint64        // round robin counter
	state      uint32        // client state
	cllock     sync.RWMutex  // lock for clients
	clients    []*Client     // connections
	nwk        string        // network type ("tcp", "udp", "unix", etc.)
	remotes    []string      // remote addresses that we can (maybe) connect to
	remoteLock sync.Mutex    // lock for remotes
	done       chan struct{} // close on close
}

// ClusterStatus is the
// connection state of
// a client connected to
// multiple endpoints.
type ClusterStatus struct {
	Network      string   // "tcp", "unix", etc.
	Connected    []string // addresses of connected servers
	Disconnected []string // addresses of disconnected servers
}

// Call makes a request on one of the available
// server connections using 'in' as the request body.
// Call blocks until the response can be read into 'out'.
func (c *ClusterClient) Call(method string, in msgp.Marshaler, out msgp.Unmarshaler) error {
next:
	v := c.next()
	if v == nil {
		if atomic.LoadUint32(&c.state) != 2 {
			return ErrClosed
		}
		err := c.dialAll()
		if err != nil {
			return err
		}
		goto next
	}
	err := v.Call(method, in, out)
	if err != nil {
		c.handleErr(v, err)
	}
	return err
}

// Async makes a request on one of the available
// server connections, but does not wait for the response.
// The returned AsyncReponse should be used to decode the
// response.
func (c *ClusterClient) Async(method string, in msgp.Marshaler) (AsyncResponse, error) {
next:
	v := c.next()
	if v == nil {
		if atomic.LoadUint32(&c.state) != 2 {
			return nil, ErrClosed
		}
		err := c.dialAll()
		if err != nil {
			return nil, err
		}
		goto next
	}
	asn, err := v.Async(method, in)
	if err != nil {
		c.handleErr(v, err)
	}
	return asn, err
}

// Add adds another connection to the client's
// connection pool.
func (c *ClusterClient) Add(addr string) error {
	if atomic.LoadUint32(&c.state) != 2 {
		return ErrClosed
	}
	return c.dial(c.nwk, addr, false)
}

// Close idempotently closes the
// client and all associated connections.
// Waiting goroutines are unblocked with an error.
func (c *ClusterClient) Close() error {
	if !atomic.CompareAndSwapUint32(&c.state, 2, 0) {
		return ErrClosed
	}
	close(c.done)

	wg := new(sync.WaitGroup)
	c.cllock.Lock()
	for _, v := range c.clients {
		wg.Add(1)
		go func(wg *sync.WaitGroup, cl *Client) {
			cl.Close()
			wg.Done()
		}(wg, v)
	}
	c.clients = c.clients[:0]
	c.cllock.Unlock()
	wg.Wait()
	return nil
}

// Status returns the connection
// status of the client.
func (c *ClusterClient) Status() *ClusterStatus {
	cc := &ClusterStatus{
		Network: c.nwk,
	}

	// this is the more "expensive"
	// lock to acquire, so we'll acquire
	// it first and then do heavy lifting
	// in the second (cheap) lock
	c.cllock.RLock()
	c.remoteLock.Lock()
	cc.Connected = make([]string, len(c.clients))
	for i, cl := range c.clients {
		cc.Connected[i] = cl.conn.RemoteAddr().String()
	}
	c.cllock.RUnlock()

	cc.Disconnected = make([]string, 0, len(c.remotes))

	for _, rem := range c.remotes {
		dialed := false
		for _, cn := range cc.Connected {
			if rem == cn {
				dialed = true
				break
			}
		}
		if !dialed {
			cc.Disconnected = append(cc.Disconnected, rem)
		}
	}
	c.remoteLock.Unlock()

	return cc
}

// acquire a client pointer via round-robin (threadsafe)
func (c *ClusterClient) next() *Client {
	i := atomic.AddUint64(&c.idx, 1)
	c.cllock.RLock()
	l := len(c.clients)
	if l == 0 {
		c.cllock.RUnlock()
		return nil
	}
	o := c.clients[i%uint64(l)]
	c.cllock.RUnlock()
	return o
}

// add a client to the list via append (threadsafe);
// doesn't add the client to the remote list
func (c *ClusterClient) add(n *Client) {
	c.cllock.Lock()
	c.clients = append(c.clients, n)
	c.cllock.Unlock()
}

// remove a client from the list (atomic); return
// whether or not is was removed so we can determine
// the winner of races to remove the client
func (c *ClusterClient) remove(n *Client) bool {
	found := false
	c.cllock.Lock()
	for i, v := range c.clients {
		if v == n {
			l := len(c.clients)
			c.clients, c.clients[i], c.clients[l-1] = c.clients[:l-1], c.clients[l-1], nil
			found = true
			break
		}
	}
	c.cllock.Unlock()
	return found
}

// redial idempotently re-dials a client.
// redial no-ops if the client isn't in the
// client list to begin with in order to prevent
// races on re-dialing.
func (c *ClusterClient) redial(n *Client) {
	// don't race on removal
	if c.remove(n) {
		n.Close()

		log.Printf("synapse cluster: re-dialing %s", n.conn.RemoteAddr())

		remote := n.conn.RemoteAddr()
		err := c.dial(remote.Network(), remote.String(), true)
		if err != nil {
			log.Printf("synapse cluster: re-dialing node @ %s failed: %s", remote.String(), err)
			return
		}
		log.Printf("synapse cluster: successfully re-connected to %s", n.conn.RemoteAddr())
	}
}

// dial; add client and remote
// if 'exists' is false, dial checks to see if the
// address is in the 'remotes' list, and if it isn't there,
// it inserts it
func (c *ClusterClient) dial(inet, addr string, exists bool) error {
	conn, err := net.Dial(inet, addr)
	if err != nil {
		return err
	}

	// this is optional b/c
	// it is *usually* unnecessary
	// and requires an expensive
	// operation while holding a lock
	if !exists {
		c.remoteLock.Lock()
		found := false
		for _, rem := range c.remotes {
			if rem == conn.RemoteAddr().String() {
				found = true
				break
			}
		}
		if !found {
			c.remotes = append(c.remotes, conn.RemoteAddr().String())
		}
		c.remoteLock.Unlock()
	}

	cl, err := NewClient(conn, 1000)
	if err != nil {
		return err
	}
	c.add(cl)
	return nil
}

// add a new connection to an existing remote
// TODO: call this on node failure
func (c *ClusterClient) addAnother() error {
	c.remoteLock.Lock()
	l := len(c.remotes)
	if l == 0 {
		c.remoteLock.Unlock()
		return ErrNoClients
	}
	this := atomic.AddUint64(&c.idx, 1)
	addr := c.remotes[this%uint64(l)]
	c.remoteLock.Unlock()

	return c.dial(c.nwk, addr, true)
}
