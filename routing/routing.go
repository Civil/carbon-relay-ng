package routing

import (
	"errors"
	"fmt"
	"github.com/Dieterbe/statsd-go"
	"github.com/graphite-ng/carbon-relay-ng/nsqd"
	"log"
	"net"
	"regexp"
	"sync"
	"time"
	"strings"
	"bytes"
	"hash/crc32"
)

type Route struct {
	// basic properties in init and copy
	Key        string         // to identify in stats/logs
	Patt       string         // regex string
	Addr       string         // tcp dest
	spoolDir   string         // where to store spool files (if enabled)
	Spool      bool           // spool metrics to disk while endpoint down?
	instrument *statsd.Client // to submit stats to

	// set automatically in init, passed on in copy
	Reg *regexp.Regexp // compiled version of patt

	// set in/via Run()
	ch           chan []byte       // to pump data to dest
	shutdown     chan bool         // signals shutdown internally
	queue        *nsqd.DiskQueue   // queue used if spooling enabled
	raddr        *net.TCPAddr      // resolved remote addr
	connUpdates  chan []*net.TCPConn // when the route connects to a new endpoint (possibly nil)
	inConnUpdate chan bool         // to signal when we start a new conn and when we finish
}

// after creating, run Run()!
func NewRoute(key, patt, addr, spoolDir string, spool bool, instrument *statsd.Client) (*Route, error) {
	route := &Route{
		Key:        key,
		Patt:       "",
		Addr:       addr,
		spoolDir:   spoolDir,
		Spool:      spool,
		instrument: instrument,
	}
	if patt == "" {
		route.Patt = ""
		route.Reg = nil
		return route, nil
	}
	err := route.updatePattern(patt)
	if err != nil {
		return nil, err
	}
	return route, nil
}

// a "basic" static copy of the route, not actually running
func (route *Route) Copy() *Route {
	return &Route{
		Key:        route.Key,
		Patt:       route.Patt,
		Addr:       route.Addr,
		spoolDir:   route.spoolDir,
		Spool:      route.Spool,
		instrument: route.instrument,
		Reg:        route.Reg,
	}
}

func (route *Route) updatePattern(pattern string) error {
	if pattern == "" {
		route.Patt = ""
		route.Reg = nil
		return nil
	}
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	route.Patt = pattern
	route.Reg = regex
	return nil
}

func (route *Route) Run() (err error) {
	route.ch = make(chan []byte)
	route.shutdown = make(chan bool)
	route.connUpdates = make(chan []*net.TCPConn)
	route.inConnUpdate = make(chan bool)
	if route.Spool {
		dqName := "spool_" + route.Key
		route.queue = nsqd.NewDiskQueue(dqName, route.spoolDir, 200*1024*1024, 1000, 2*time.Second).(*nsqd.DiskQueue)
	}
	go route.relay()
	return err
}

func (route *Route) Shutdown() error {
	if route.shutdown == nil {
		return errors.New("not running yet")
	}
	route.shutdown <- true
	return nil
}

func (route *Route) updateConn(addrs string) error {
	log.Printf("%v (re)connecting to %v\n", route.Key, addrs)
	route.inConnUpdate <- true
	defer func() { route.inConnUpdate <- false }()
	addrs_list := strings.Split(addrs, ",")
	new_conns := make([]*net.TCPConn, len(addrs_list))
	i := 0
	for _, addr := range addrs_list {
		raddr, err := net.ResolveTCPAddr("tcp", addr)
		if nil != err {
			log.Printf("%v resolve failed: %s\n", route.Key, err.Error())
			return err
		}
		laddr, _ := net.ResolveTCPAddr("tcp", "0.0.0.0")
		new_conn, err := net.DialTCP("tcp", laddr, raddr)
		if nil != err {
			log.Printf("%v connect failed: %s\n", route.Key, err.Error())
			return err
		}
		log.Printf("%v connected to %s\n", route.Key, addr)
		new_conns[i] = new_conn
		i++
	}
	if addrs != route.Addr {
		log.Printf("%v update address to %v (%v)\n", route.Key, addrs, route.Addr)
		route.Addr = addrs
	}
	route.connUpdates <- new_conns
	return nil
}

// TODO func (l *TCPListener) SetDeadline(t time.Time)
// TODO Decide when to drop this buffer and move on.
func (route *Route) relay() {
	period_assure_conn := time.Duration(60) * time.Second
	ticker := time.NewTicker(period_assure_conn)
	var to_unspool chan []byte
	var conns []*net.TCPConn
	var conns_len uint32
	var conn *net.TCPConn

	process_packet := func(buf []byte) {
		if conns == nil {
			if route.Spool {
				route.instrument.Increment("route=" + route.Key + ".target_type=count.unit=Metric.direction=spool")
				route.queue.Put(buf)
			} else {
				// note, we drop packets while we set up connection
				route.instrument.Increment("route=" + route.Key + ".target_type=count.unit=Metric.direction=drop")
			}
			return
		}
		n := bytes.IndexAny(buf, " ")
		if (conns_len == 1) {
			conn = conns[0]
		} else if n != -1 {
			conn = conns[crc32.ChecksumIEEE(buf[:n]) % conns_len]
			route.instrument.Increment("route=" + route.Key + ".target_type=count.unit=Metric.direction=out")
		} else {
			log.Println("Malformed input detected, refusing to process packet: %s", string(buf))
			return
		}
		n, err := conn.Write(buf)
		if nil != err {
			route.instrument.Increment("route=" + route.Key + ".target_type=count.unit=Err")
			log.Println(err)
			conn.Close()
			conn = nil
			if route.Spool {
				fmt.Println("writing to spool")
				route.queue.Put(buf)
			}
			return
		}
		if len(buf) != n {
			route.instrument.Increment("route=" + route.Key + ".target_type=count.unit=Err")
			log.Printf(route.Key+" truncated: %s\n", buf)
			conn.Close()
			conn = nil
			if route.Spool {
				fmt.Println("writing to spool")
				route.queue.Put(buf)
			}
		}
	}

	conn_updates := 0
	go route.updateConn(route.Addr)

	for {
		// only process spool queue if we have an outbound connection
		if conns != nil && route.Spool {
			to_unspool = route.queue.ReadChan()
		} else {
			to_unspool = nil
		}

		select {
		case inConnUpdate := <-route.inConnUpdate:
			if inConnUpdate {
				conn_updates += 1
			} else {
				conn_updates -= 1
			}
		case new_conns := <-route.connUpdates:
			conns = new_conns // can be nil and that's ok (it means we had to [re]connect but couldn't)
			conns_len = uint32(len(conns))
		case <-ticker.C: // periodically try to bring connection (back) up, if we have to, and no other connect is happening
			if conns == nil && conn_updates == 0 {
				go route.updateConn(route.Addr)
			}
		case <-route.shutdown:
			//fmt.Println(route.Key + " route relay -> requested shutdown. quitting")
			return
		case buf := <-to_unspool:
			process_packet(buf)
		case buf := <-route.ch:
			process_packet(buf)
		}
	}

}

type Routes struct {
	Map      map[string]*Route
	lock     sync.Mutex
	SpoolDir string
}

func NewRoutes(routeDefsMap map[string]*Route, spoolDir string, instrument *statsd.Client) (routes *Routes, err error) {
	routesMap := make(map[string]*Route)
	for k, routeDef := range routeDefsMap {
		route, err := NewRoute(k, routeDef.Patt, routeDef.Addr, spoolDir, routeDef.Spool, instrument)
		if err != nil {
			return nil, err
		}
		routesMap[k] = route
	}
	routes = &Routes{Map: routesMap, SpoolDir: spoolDir}
	return routes, nil
}

// not thread safe, run this once only
func (routes *Routes) Run() error {
	for _, route := range routes.Map {
		err := route.Run()
		if nil != err {
			return err
		}
	}
	return nil
}
func (routes *Routes) Dispatch(buf []byte, first_only bool) (routed bool) {
	//fmt.Println("entering dispatch")
	routes.lock.Lock()
	defer routes.lock.Unlock()
	for _, route := range routes.Map {
		if route.Reg == nil || route.Reg.Match(buf) {
			routed = true
			//fmt.Println("routing to " + route.Key)
			route.ch <- buf
			if first_only {
				break
			}
		}
	}
	//fmt.Println("Dispatched")
	return routed
}

func (routes *Routes) List() map[string]Route {
	ret := make(map[string]Route)
	routes.lock.Lock()
	defer routes.lock.Unlock()
	for k, v := range routes.Map {
		ret[k] = *v.Copy()
	}
	return ret
}

func (routes *Routes) Add(key, patt, addr string, spool bool, instrument *statsd.Client) error {
	routes.lock.Lock()
	defer routes.lock.Unlock()
	_, found := routes.Map[key]
	if found {
		return errors.New("route with given key already exists")
	}
	route, err := NewRoute(key, patt, addr, routes.SpoolDir, spool, instrument)
	if err != nil {
		return err
	}
	err = route.Run()
	if err != nil {
		return err
	}
	routes.Map[key] = route
	return nil
}

func (routes *Routes) Update(key string, addr, patt *string) error {
	routes.lock.Lock()
	defer routes.lock.Unlock()
	route, found := routes.Map[key]
	if !found {
		return errors.New("unknown route '" + key + "'")
	}
	if patt != nil {
		err := route.updatePattern(*patt)
		if err != nil {
			return err
		}
	}
	if addr != nil {
		return route.updateConn(*addr)
	}
	return nil
}

func (routes *Routes) Del(key string) error {
	routes.lock.Lock()
	defer routes.lock.Unlock()
	route, found := routes.Map[key]
	if !found {
		return errors.New("unknown route '" + key + "'")
	}
	delete(routes.Map, key)
	err := route.Shutdown()
	if err != nil {
		// route removed from routing table but still trying to connect
		// it won't get new stuff on its input though
		return err
	}
	return nil
}
