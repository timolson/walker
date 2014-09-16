package walker

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"time"

	"code.google.com/p/go.net/publicsuffix"
	"code.google.com/p/log4go"

	"github.com/gocql/gocql"
)

// Datastore defines the interface for an object to be used as walker's datastore.
//
// Note that this is for link and metadata storage required to make walker
// function properly. It has nothing to do with storing fetched content (see
// `Handler` for that).
type Datastore interface {
	// ClaimNewHost returns a hostname that is now claimed for this crawler to
	// crawl. A segment of links for this host is assumed to be available.
	// Returns the domain of the segment it claimed, or "" if there are none
	// available.
	ClaimNewHost() string

	// UnclaimHost indicates that all links from `LinksForHost` have been
	// processed, so other work may be done with this host. For example the
	// dispatcher will be free analyze the links and generate a new segment.
	UnclaimHost(host string)

	// LinksForHost returns a channel that will feed URLs for a given host.
	LinksForHost(host string) <-chan *url.URL

	// StoreURLFetchResults takes the return data/metadata from a fetch and
	// stores the visit. Fetchers will call this once for each link in the
	// segment being crawled.
	StoreURLFetchResults(fr *FetchResults)

	// StoreParsedURL stores a URL parsed out of a page (i.e. a URL we may not
	// have crawled yet). `u` is the URL to store. `res` is the FetchResults
	// object for the fetch from which we got the URL, for any context the
	// datastore may want.
	//
	// This layer should handle efficiently deduplicating
	// links (i.e. a fetcher should be safe feeding the same URL many times.
	StoreParsedURL(u *url.URL, fr *FetchResults)
}

// CassandraDatastore is the primary Datastore implementation, using Apache
// Cassandra as a highly scalable backend.
type CassandraDatastore struct {
	cf            *gocql.ClusterConfig
	db            *gocql.Session
	cachedDomains []string
}

func GetCassandraConfig() *gocql.ClusterConfig {
	config := gocql.NewCluster(Config.Cassandra.Hosts[0])
	config.Keyspace = Config.Cassandra.Keyspace
	return config
}

func NewCassandraDatastore(cf *gocql.ClusterConfig) (*CassandraDatastore, error) {
	ds := new(CassandraDatastore)
	ds.cf = cf
	var err error
	ds.db, err = ds.cf.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("Failed to create cassandra datastore: %v", err)
	}
	return ds, nil
}

func (ds *CassandraDatastore) Close() {
	ds.db.Close()
}

func (ds *CassandraDatastore) ClaimNewHost() string {

	// Get our range of priority values and sort high to low
	// Currently simplified to one level top optimize fake crawler
	priorities := []int{0}

	//priorities := []int{}
	//var p int
	//priority_iter := ds.db.Query(`SELECT DISTINCT priority FROM domains_to_crawl`).Iter()
	//defer priority_iter.Close()
	//for priority_iter.Scan(&p) {
	//	priorities = append(priorities, p)
	//}
	//sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	if len(ds.cachedDomains) == 0 {
		// Start with the highest priority selecting until we find an unclaimed domain segment,
		// then claim it
		start := time.Now()
		var domain string
		for _, p := range priorities {
			domain_iter := ds.db.Query(`SELECT domain FROM domains_to_crawl
										WHERE priority = ?
										AND crawler_token = 00000000-0000-0000-0000-000000000000
										LIMIT 50`, p).Iter()
			defer domain_iter.Close()
			for domain_iter.Scan(&domain) {
				//TODO: use lightweight transaction to allow more crawlers
				//TODO: use a per-crawler uuid
				log4go.Info("ClaimNextDomain selected new domain in %v", time.Since(start))
				start = time.Now()
				crawluuid, _ := gocql.RandomUUID()
				err := ds.db.Query(`UPDATE domains_to_crawl SET crawler_token = ?, claim_time = ?
									WHERE priority = ? AND domain = ?`,
					crawluuid, time.Now(), p, domain).Exec()
				if err != nil {
					log4go.Error("Failed to claim segment %v: %v", domain, err)
				} else {
					log4go.Info("Claimed segment %v with token %v in %v", domain, crawluuid, time.Since(start))
					ds.cachedDomains = append(ds.cachedDomains, domain)
				}
			}
		}
	}

	if len(ds.cachedDomains) > 0 {
		// Pop the last element and return it
		lastIndex := len(ds.cachedDomains) - 1
		domain := ds.cachedDomains[lastIndex]
		ds.cachedDomains = ds.cachedDomains[:lastIndex]
		return domain
	} else {
		return ""
	}
}

func (ds *CassandraDatastore) UnclaimHost(host string) {
	err := ds.db.Query(`DELETE FROM segments WHERE domain = ?`, host).Exec()
	if err != nil {
		log4go.Error("Failed deleting segment links for %v: %v", host, err)
	}

	// Since (priority, domain) is the primary key we need to select the priority
	// first in order to delete. https://issues.apache.org/jira/browse/CASSANDRA-5527
	var priority int
	err = ds.db.Query(`SELECT priority FROM domains_to_crawl WHERE domain = ?`, host).Scan(&priority)
	if err != nil {
		log4go.Error("Failed getting priority for %v: %v", host, err)
		return
	}
	err = ds.db.Query(`DELETE FROM domains_to_crawl WHERE priority = ? AND domain = ?`,
		priority, host).Exec()
	if err != nil {
		log4go.Error("Failed deleting %v from domains_to_crawl: %v", host, err)
	}
}

func (ds *CassandraDatastore) LinksForHost(domain string) <-chan *url.URL {
	links, err := ds.getSegmentLinks(domain)
	if err != nil {
		log4go.Error("Failed to grab segment for %v: %v", domain, err)
		return nil
	}
	log4go.Info("Returning %v links to crawl domain %v", len(links), domain)

	linkchan := make(chan *url.URL, len(links))
	for _, l := range links {
		linkchan <- l
	}
	close(linkchan)
	return linkchan
}

func (ds *CassandraDatastore) StoreURLFetchResults(fr *FetchResults) {
	u := fr.Url
	domain, err := publicsuffix.EffectiveTLDPlusOne(u.Host)
	subdomain := strings.TrimSuffix(u.Host, domain)

	if fr.FetchError != nil {
		//TODO
	}

	if fr.ExcludedByRobots {
		//TODO: populate robots_excluded
	}

	//TODOs here due to gocql's inability to allow nils, find some other way to do it.
	//TODO: redirectURL, _ := fr.Res.Location()

	err = ds.db.Query(
		`INSERT INTO links (domain, subdomain, path, protocol, crawl_time, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		domain,
		subdomain,
		u.Path,
		u.Scheme,
		fr.FetchTime,
		fr.Res.StatusCode,
		//TODO: error -- fr.FetchError,
		//TODO: fp
		//TODO: can we get RemoteAddr? fr.Res.Request.RemoteAddr may not be filled in
		//TODO: fr.Res.Header.Get("Content-Type"),
	).Exec()
	if err != nil {
		log4go.Error("Failed storing fetch results: %v", err)
	}
}

func (ds *CassandraDatastore) StoreParsedURL(u *url.URL, fr *FetchResults) {
	if u.Host == "" {
		log4go.Warn("Not handling link because there is no host: %v", *u)
		return
	}
	ds.addDomainIfNew(u.Host)
	err := ds.db.Query(`INSERT INTO links (domain, subdomain, path, protocol, crawl_time)
						VALUES (?, ?, ?, ?, ?)`, u.Host, "", u.Path, u.Scheme, time.Unix(0, 0)).Exec()
	if err != nil {
		log4go.Error("failed inserting parsed url (%v) to cassandra, %v", u, err)
	}
}

func (ds *CassandraDatastore) addDomainIfNew(domain string) {
	var count int
	err := ds.db.Query(`SELECT COUNT(*) FROM domain_info WHERE domain = ?`, domain).Scan(&count)
	if err != nil {
		log4go.Error("Failed to check if %v is in domain_info: %v", domain, err)
		return // with error, assume we already have it and move on
	}
	if count == 0 {
		err := ds.db.Query(`INSERT INTO domain_info (domain) VALUES (?)`, domain).Exec()
		if err != nil {
			log4go.Error("Failed to add new domain %v: %v", domain, err)
		}
	}
}

func (ds *CassandraDatastore) getSegmentLinks(domain string) (links []*url.URL, err error) {
	q := ds.db.Query(`SELECT domain, subdomain, path, protocol, crawl_time
						FROM segments WHERE domain = ?`, domain)
	iter := q.Iter()
	defer func() { err = iter.Close() }()

	var dbdomain, subdomain, path, protocol string
	var crawl_time time.Time
	for iter.Scan(&dbdomain, &subdomain, &path, &protocol, &crawl_time) {
		if subdomain != "" {
			subdomain = subdomain + "."
		}
		link := fmt.Sprintf("%s://%s%s/%s", protocol, subdomain, dbdomain, path)
		u, e := url.Parse(link)
		if e != nil {
			log4go.Error("Error adding link (%v) to crawl: %v", link, e)
		} else {
			log4go.Debug("Adding link: %v", u)
			links = append(links, u)
		}
	}
	return
}

// ToURL creates a *url.URL out of our commonly used column values.
func ToURL(domain, subdomain, path, protocol string) (*url.URL, error) {
	// Make sure the subdomain ends in '.' if it exists
	if subdomain != "" && !strings.HasSuffix(subdomain, ".") {
		subdomain = subdomain + "."
	}

	// Make sure the path starts in '/' if it exists
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	l := fmt.Sprintf("%s://%s%s%s", protocol, subdomain, domain, path)
	u, err := url.Parse(l)
	if err != nil {
		return nil, fmt.Errorf("Couldn't create a *url.URL "+
			"from domain: %v, subdomain: %v, path: %v, protocol: %v -- error: %v",
			domain, subdomain, path, protocol, err)
	}
	return u, nil
}

// createCassandraSchema creates the walker schema in the configured Cassandra
// database. It requires that the keyspace not already exist (so as to losing
// non-test data), with the exception of the walker_test schema, which it will
// drop automatically.
func CreateCassandraSchema() error {
	config := GetCassandraConfig()
	config.Keyspace = ""
	db, err := config.CreateSession()
	if err != nil {
		return fmt.Errorf("Could not connect to create cassandra schema: %v", err)
	}

	if Config.Cassandra.Keyspace == "walker_test" {
		err := db.Query("DROP KEYSPACE IF EXISTS walker_test").Exec()
		if err != nil {
			return fmt.Errorf("Failed to drop walker_test keyspace: %v", err)
		}
	}

	t, err := template.New("schema").Parse(schemaTemplate)
	if err != nil {
		return fmt.Errorf("Failure parsing the CQL schema template: %v", err)
	}
	var b bytes.Buffer
	t.Execute(&b, Config.Cassandra)

	for _, q := range strings.Split(b.String(), ";") {
		err = db.Query(q).Exec()
		if err != nil {
			return fmt.Errorf("Failed to create schema: %v\nStatement:\n%v", err, q)
		}
	}
	return nil
}

const schemaTemplate string = `-- The schema file for walker
--
-- This file gets generated from a Go template so the keyspace and replication
-- can be configured (particularly for testing purposes)
CREATE KEYSPACE {{.Keyspace}}
WITH REPLICATION = { 'class': 'NetworkTopologyStrategy', 'DC1': {{.ReplicationFactor}} };

CREATE TABLE {{.Keyspace}}.links (
  domain text, -- "google.com"
  subdomain text, --  "www" (does not include .)
  path text, -- "/index.hml"
  protocol text, -- "http"
  crawl_time timestamp, -- 0/epoch indicates initial insert (not yet fetched)
  --port int,

  status int,
  error text,
  fp bigint,
  referer text,
  redirect_url text,
  ip text,
  mime text,
  encoding text,
  robots_excluded boolean,
  PRIMARY KEY (domain, subdomain, path, protocol, crawl_time)
) WITH compaction = { 'class' : 'LeveledCompactionStrategy' };

CREATE TABLE {{.Keyspace}}.segments (
  domain text,
  subdomain text,
  path text,
  protocol text,
  --port int,

  crawl_time timestamp,
  PRIMARY KEY (domain, subdomain, path, protocol)
) WITH compaction = { 'class' : 'LeveledCompactionStrategy' };

CREATE TABLE {{.Keyspace}}.domain_info (
  domain text,
  excluded boolean,
  exclude_reason text,
  mirror_for text,
  PRIMARY KEY (domain)
) WITH compaction = { 'class' : 'LeveledCompactionStrategy' };

CREATE TABLE {{.Keyspace}}.domains_to_crawl (
  priority int,
  domain text,
  crawler_token uuid,
  claim_time timestamp,
  PRIMARY KEY (priority, domain)
) WITH compaction = { 'class' : 'LeveledCompactionStrategy' };
CREATE INDEX domains_to_crawl_crawler_token
  ON {{.Keyspace}}.domains_to_crawl (crawler_token);
CREATE INDEX domains_to_crawl_domain
  ON {{.Keyspace}}.domains_to_crawl (domain)`