package walker

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"strings"
	"sync"

	"github.com/iParadigms/walker/mimetools"

	"github.com/temoto/robotstxt.go"

	"net"
	"net/http"
	"net/url"
	"time"

	"code.google.com/p/go.net/html"
	"code.google.com/p/go.net/html/charset"
	"code.google.com/p/go.net/publicsuffix"
	"code.google.com/p/log4go"
)

// NotYetCrawled is a convenience for time.Unix(0, 0), used as a crawl time in
// Walker for links that have not yet been fetched.
var NotYetCrawled time.Time

func init() {
	NotYetCrawled = time.Unix(0, 0)
}

// FetchResults contains all relevant context and return data from an
// individual fetch. Handlers receive this to process results.
type FetchResults struct {
	// URL that was fetched; will always be populated
	URL *URL

	// Response object; nil if there was a FetchError or ExcludedByRobots is
	// true. Response.Body may not be the same object the HTTP request actually
	// returns; the fetcher may have read in the response to parse out links,
	// replacing Response.Body with an alternate reader.
	Response *http.Response

	// FetchError if the net/http request had an error (non-2XX HTTP response
	// codes are not considered errors)
	FetchError error

	// Time at the beginning of the request (if a request was made)
	FetchTime time.Time

	// True if we did not request this link because it is excluded by
	// robots.txt rules
	ExcludedByRobots bool
}

// URL is the walker URL object, which embeds *url.URL but has extra data and
// capabilities used by walker. Note that LastCrawled should not be set to its
// zero value, it should be set to NotYetCrawled.
type URL struct {
	*url.URL

	// LastCrawled is the last time we crawled this URL, for example to use a
	// Last-Modified header.
	LastCrawled time.Time
}

// CreateURL creates a walker URL from values usually pulled out of the
// datastore. subdomain may optionally include a trailing '.', and path may
// optionally include a prefixed '/'.
func CreateURL(domain, subdomain, path, protocol string, lastcrawled time.Time) (*URL, error) {
	if subdomain != "" && !strings.HasSuffix(subdomain, ".") {
		subdomain = subdomain + "."
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	ref := fmt.Sprintf("%s://%s%s%s", protocol, subdomain, domain, path)
	u, err := ParseURL(ref)
	if err != nil {
		return nil, err
	}
	u.LastCrawled = lastcrawled
	return u, nil
}

// ParseURL is the walker.URL equivalent of url.Parse
func ParseURL(ref string) (*URL, error) {
	u, err := url.Parse(ref)
	return &URL{URL: u, LastCrawled: NotYetCrawled}, err
}

// ToplevelDomainPlusOne returns the Effective Toplevel Domain of this host as
// defined by https://publicsuffix.org/, plus one extra domain component.
//
// For example the TLD of http://www.bbc.co.uk/ is 'co.uk', plus one is
// 'bbc.co.uk'. Walker uses these TLD+1 domains as the primary unit of
// grouping.
func (u *URL) ToplevelDomainPlusOne() string {
	domain, err := publicsuffix.EffectiveTLDPlusOne(u.Host)
	if err != nil {
		log4go.Error("Error trying to get TLD+1 from %v, error: %v", *u, err)
		return u.Host
	}
	return domain
}

// Subdomain provides the remaining subdomain after removing the
// ToplevelDomainPlusOne. For example http://www.bbc.co.uk/ will return 'www'
// as the subdomain (note that there is no trailing period). If there is no
// subdomain it will return "".
func (u *URL) Subdomain() string {
	tld := u.ToplevelDomainPlusOne()
	if len(u.Host) == len(tld) {
		return ""
	}
	return strings.TrimSuffix(u.Host, "."+tld)
}

// FetchManager configures and runs the crawl.
//
// The calling code must create a FetchManager, set a Datastore and handlers,
// then call `Start()`
type FetchManager struct {
	// Handler must be set to handle fetch responses.
	Handler Handler

	// Datastore must be set to drive the fetching.
	Datastore Datastore

	// Transport can be set to override the default network transport the
	// FetchManager is going to use. Good for faking remote servers for
	// testing.
	Transport http.RoundTripper

	fetchers  []*fetcher
	fetchWait sync.WaitGroup
	started   bool

	// used to match Content-Type headers
	acceptFormats *mimetools.Matcher
}

// Start begins processing assuming that the datastore and any handlers have
// been set. This is a blocking call (run in a goroutine if you want to do
// other things)
//
// You cannot change the datastore or handlers after starting.
func (fm *FetchManager) Start() {
	log4go.Info("Starting FetchManager")
	if fm.Datastore == nil {
		panic("Cannot start a FetchManager without a datastore")
	}
	if fm.Handler == nil {
		panic("Cannot start a FetchManager without a handler")
	}
	if fm.started {
		panic("Cannot start a FetchManager multiple times")
	}

	mm, err := mimetools.NewMatcher(Config.AcceptFormats)
	fm.acceptFormats = mm
	if err != nil {
		panic(fmt.Errorf("mimetools.NewMatcher failed to initialize: %v", err))
	}

	fm.started = true

	t, ok := fm.Transport.(*http.Transport)
	if ok {
		t.Dial = DNSCachingDial(t.Dial, Config.MaxDNSCacheEntries)
	} else {
		log4go.Info("Given an non-http transport, not using dns caching")
	}

	numFetchers := Config.NumSimultaneousFetchers
	fm.fetchers = make([]*fetcher, numFetchers)
	for i := 0; i < numFetchers; i++ {
		f := newFetcher(fm)
		fm.fetchers[i] = f
		fm.fetchWait.Add(1)
		go func() {
			f.start()
			fm.fetchWait.Done()
		}()
	}
	fm.fetchWait.Wait()
}

// Stop notifies the fetchers to finish their current requests. It blocks until
// all fetchers have finished.
func (fm *FetchManager) Stop() {
	log4go.Info("Stopping FetchManager")
	if !fm.started {
		panic("Cannot stop a FetchManager that has not been started")
	}
	for _, f := range fm.fetchers {
		go f.stop()
	}
	fm.fetchWait.Wait()
}

// fetcher encompasses one of potentially many fetchers the FetchManager may
// start up. It will effectively manage one goroutine, crawling one host at a
// time, claiming a new host when it has exhausted the previous one.
type fetcher struct {
	fm         *FetchManager
	host       string
	httpclient *http.Client
	robots     *robotstxt.Group
	crawldelay time.Duration

	// quit signals the fetcher to stop
	quit chan struct{}

	// done receives when the fetcher has finished; this is necessary because
	// the fetcher may need to clean up (ex. unclaim the current host) after
	// reading from quit
	done chan struct{}
}

func newFetcher(fm *FetchManager) *fetcher {
	f := new(fetcher)
	f.fm = fm
	f.httpclient = &http.Client{
		Transport: fm.Transport,
	}
	f.quit = make(chan struct{})
	f.done = make(chan struct{})
	return f
}

// start blocks until the fetcher has completed by being told to quit.
func (f *fetcher) start() {
	log4go.Debug("Starting new fetcher")
	for {
		if f.host != "" {
			//TODO: ensure that this unclaim will happen... probably want the
			//logic below in a function where the Unclaim is deferred
			f.fm.Datastore.UnclaimHost(f.host)
		}

		select {
		case <-f.quit:
			f.done <- struct{}{}
			return
		default:
		}

		f.host = f.fm.Datastore.ClaimNewHost()
		if f.host == "" {
			time.Sleep(time.Second)
			continue
		}

		if f.checkForBlacklisting(f.host) {
			continue
		}

		f.fetchRobots(f.host)
		f.crawldelay = time.Duration(Config.DefaultCrawlDelay) * time.Second
		if f.robots != nil && int(f.robots.CrawlDelay) > Config.DefaultCrawlDelay {
			f.crawldelay = f.robots.CrawlDelay
		}
		log4go.Debug("Crawling host: %v with crawl delay %v", f.host, f.crawldelay)

		for link := range f.fm.Datastore.LinksForHost(f.host) {

			//TODO: check <-f.quit and clean up appropriately

			fr := &FetchResults{URL: link}

			if f.robots != nil && !f.robots.Test(link.String()) {
				fr.ExcludedByRobots = true
				f.fm.Datastore.StoreURLFetchResults(fr)
				continue
			}

			time.Sleep(f.crawldelay)

			fr.FetchTime = time.Now()
			fr.Response, fr.FetchError = f.fetch(link)
			if fr.FetchError != nil {
				log4go.Debug("Error fetching %v: %v", link, fr.FetchError)
				f.fm.Datastore.StoreURLFetchResults(fr)
				continue
			}

			log4go.Debug("Fetched %v -- %v", link, fr.Response.Status)

			canSearch := isHTML(fr.Response)
			if canSearch {
				log4go.Debug("Reading and parsing as HTML (%v)", link)

				//TODO: ReadAll is inefficient. We should use a properly sized
				//		buffer here (determined by
				//		Config.MaxHTTPContentSizeBytes or possibly
				//		Content-Length of the response)
				var body []byte
				body, fr.FetchError = ioutil.ReadAll(fr.Response.Body)
				if fr.FetchError != nil {
					log4go.Debug("Error reading body of %v: %v", link, fr.FetchError)
					f.fm.Datastore.StoreURLFetchResults(fr)
					continue
				}
				fr.Response.Body = ioutil.NopCloser(bytes.NewReader(body))

				outlinks, err := getLinks(body)
				if err != nil {
					log4go.Warn("error parsing HTML for page %v: %v", link, err)
				} else {
					for _, outlink := range outlinks {
						if outlink.Scheme == "" {
							outlink.Scheme = link.Scheme
						}
						if outlink.Host == "" {
							outlink.Host = link.Host
						}
						log4go.Debug("Parsed link: %v", outlink)
						f.fm.Datastore.StoreParsedURL(outlink, fr)
					}
				}
			}

			// handle any doc that we searched or that is in our AcceptFormats
			// list
			canHandle := isHandleable(fr.Response, f.fm.acceptFormats)
			if canSearch || canHandle {
				f.fm.Handler.HandleResponse(fr)
			} else {
				ctype := strings.Join(fr.Response.Header["Content-Type"], ",")
				log4go.Debug("Not handling url %v -- `Content-Type: %v`", fr.URL.String(), ctype)
			}

			//TODO: Wrap the reader and check for read error here
			f.fm.Datastore.StoreURLFetchResults(fr)

		}
	}
}

// stop signals a fetcher to stop and waits until completion.
func (f *fetcher) stop() {
	f.quit <- struct{}{}
	<-f.done
}

func (f *fetcher) fetchRobots(host string) {
	u := &URL{
		URL: &url.URL{
			Scheme: "http",
			Host:   host,
			Path:   "robots.txt",
		},
	}
	res, err := f.fetch(u)
	if err != nil {
		log4go.Info("Could not fetch %v, assuming there is no robots.txt (error: %v)", u, err)
		f.robots = nil
		return
	}
	robots, err := robotstxt.FromResponse(res)
	res.Body.Close()
	if err != nil {
		log4go.Info("Error parsing robots.txt (%v) assuming there is no robots.txt: %v", u, err)
		f.robots = nil
		return
	}
	f.robots = robots.FindGroup(Config.UserAgent)
}

func (f *fetcher) fetch(u *URL) (*http.Response, error) {
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("Failed to create new request object for %v): %v", u, err)
	}

	req.Header.Set("User-Agent", Config.UserAgent)
	req.Header.Set("Accept", strings.Join(Config.AcceptFormats, ","))
	//TODO: set headers? req.Header[] = ...

	// Do the request.
	res, err := f.httpclient.Do(req)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// checkForBlacklisting returns true if this site is blacklisted or should be
// blacklisted. If we detect that this site should be blacklisted, this
// function will call the datastore appropriately.
//
// One example of blacklisting is detection of IP addresses that resolve to
// localhost or other bad IP ranges.
func (f *fetcher) checkForBlacklisting(host string) bool {
	dialer, ok := f.fm.Transport.(interface {
		Dial(network, addr string) (net.Conn, error)
	})
	if !ok {
		// If the transport doesn't have Dial then don't block it; we can't get
		// the IP address
		return false
	}

	conn, err := dialer.Dial("tcp", host)
	if err != nil {
		//TODO: blacklist this domain in the datastore as couldn't connect;
		//maybe try a few times
		log4go.Info("Could not connect to host (%v), blacklisting", host)
		return true
	}
	defer conn.Close()

	if Config.BlacklistPrivateIPs && isPrivateAddr(conn.RemoteAddr().String()) {
		//TODO: mark this domain as blacklisted  in the datastore for resolving
		//to a private IP
		log4go.Info("Host (%v) resolved to private IP address, blacklisting", host)
		return true
	}
	return false
}

// getLinks parses the response for links, doing it's best with bad HTML.
func getLinks(contents []byte) ([]*URL, error) {
	utf8Reader, err := charset.NewReader(bytes.NewReader(contents), "text/html")
	if err != nil {
		return nil, err
	}
	tokenizer := html.NewTokenizer(utf8Reader)

	var links []*URL
	tags := getIncludedTags()

	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			//TODO: should use tokenizer.Err() to see if this is io.EOF
			//		(meaning success) or an actual error
			return links, nil
		case html.StartTagToken:

			tagName, hasAttrs := tokenizer.TagName()
			if hasAttrs && tags[string(tagName)] {
				links = parseAnchorAttrs(tokenizer, links)
			}
		}
	}

	return links, nil
}

// getIncludedTags gets a map of tags we should check for outlinks. It uses
// ignored_tags in the config to exclude ones we don't want. Tags are []byte
// types (not strings) because []byte is what the parser uses.
func getIncludedTags() map[string]bool {
	tags := map[string]bool{
		"a":      true,
		"area":   true,
		"form":   true,
		"frame":  true,
		"iframe": true,
		"script": true,
		"link":   true,
		"img":    true,
	}
	for _, t := range Config.IgnoreTags {
		delete(tags, t)
	}
	return tags
}

// parseAnchorAttrs iterates over all of the attributes in the current anchor token.
// If a href is found, it adds the link value to the links slice.
// Returns the new link slice.
func parseAnchorAttrs(tokenizer *html.Tokenizer, links []*URL) []*URL {
	//TODO: rework this to be cleaner, passing in `links` to be appended to
	//isn't great
	for {
		key, val, moreAttr := tokenizer.TagAttr()
		if bytes.Compare(key, []byte("href")) == 0 {
			u, err := ParseURL(string(val))
			if err == nil {
				links = append(links, u)
			}
		}
		if !moreAttr {
			return links
		}
	}
}

func isHTML(r *http.Response) bool {
	if r == nil {
		return false
	}
	for _, ct := range r.Header["Content-Type"] {
		if strings.HasPrefix(ct, "text/html") {
			return true
		}
	}
	return false
}

func isHandleable(r *http.Response, mm *mimetools.Matcher) bool {
	for _, ct := range r.Header["Content-Type"] {
		matched, err := mm.Match(ct)
		if err == nil && matched {
			return true
		}
	}
	return false
}

var privateNetworks = []*net.IPNet{
	parseCIDR("10.0.0.0/8"),
	parseCIDR("192.168.0.0/16"),
	parseCIDR("172.16.0.0/12"),
	parseCIDR("127.0.0.0/8"),
}

// parseCIDR is a convenience for creating our static private IPNet ranges
func parseCIDR(netstring string) *net.IPNet {
	_, network, err := net.ParseCIDR(netstring)
	if err != nil {
		panic(err.Error())
	}
	return network
}

// isPrivateAddr determines whether the input address belongs to any of the
// private networks specified in privateNetworkStrings. It returns an error
// if the input string does not represent an IP address.
func isPrivateAddr(addr string) bool {
	// Remove the port number if there is one
	if index := strings.LastIndex(addr, ":"); index != -1 {
		addr = addr[:index]
	}

	thisIP := net.ParseIP(addr)
	if thisIP == nil {
		log4go.Error("Failed to parse as IP address: %v", addr)
		return false
	}
	for _, network := range privateNetworks {
		if network.Contains(thisIP) {
			return true
		}
	}
	return false
}
