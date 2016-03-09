// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Contributor:
// - Aaron Meihm ameihm@mozilla.com

package main

import (
	"code.google.com/p/gcfg"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gorilla/context"
	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"net/http"
	"os"
	"os/signal"
	slib "servicelib"
	"strings"
	"sync"
	"syscall"
	"time"
)

var pidFile string
var pidFD *os.File

type opContext struct {
	tx    *sql.Tx
	db    *sql.DB
	opid  string
	rhost string
}

func (o *opContext) newContext(db *sql.DB, useTransaction bool, rhost string) (err error) {
	o.opid = slib.NewUUID()
	o.db = db
	o.rhost = rhost
	if useTransaction {
		o.tx, err = db.Begin()
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *opContext) Query(qs string, args ...interface{}) (*sql.Rows, error) {
	if o.tx != nil {
		return o.tx.Query(qs, args...)
	}
	return o.db.Query(qs, args...)
}

func (o *opContext) Exec(qs string, args ...interface{}) (sql.Result, error) {
	if o.tx != nil {
		return o.tx.Exec(qs, args...)
	}
	return o.db.Exec(qs, args...)
}

func (o *opContext) commit() error {
	if o.tx == nil {
		return nil
	}
	return o.tx.Commit()
}

func (o *opContext) rollback() error {
	return o.tx.Rollback()
}

func (o *opContext) logf(s string, args ...interface{}) {
	buf := fmt.Sprintf(s, args...)
	clnt := "none"
	if o.rhost != "" {
		clnt = o.rhost
	}
	logf("[%v:%v] %v", o.opid, clnt, buf)
}

type Config struct {
	General struct {
		Listen string
		Key    string
		Cert   string
	}
	Database struct {
		Hostname string
		Database string
	}
	Vulnerabilities struct {
		ESHost string
		Index  string
	}
	Compliance struct {
		ESHost           string
		Index            string
		ScoringBatchSize int
		ScoreEvery       string
	}
}

func (c *Config) validate() error {
	if c.General.Listen == "" {
		return fmt.Errorf("missing configuration option: general..listen")
	}
	if c.Database.Hostname == "" {
		return fmt.Errorf("missing configuration option: database..hostname")
	}
	if c.Database.Database == "" {
		return fmt.Errorf("missing configuration option: database..database")
	}
	return nil
}

var cfg Config
var dbconn *sql.DB

var wg sync.WaitGroup
var logChan chan string

func serviceLookup(op opContext, s *slib.Service) error {
	useid := s.SystemGroup.ID
	rows, err := op.Query(`SELECT service, rraid,
		ari, api, afi, cri, cpi, cfi,
		iri, ipi, ifi,
		arp, app, afp,
		crp, cpp, cfp,
		irp, ipp, ifp,
		datadefault
		FROM rra
		WHERE rraid IN (
		SELECT rraid FROM rra_sysgroup
		WHERE sysgroupid = $1 )`, useid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ns slib.RRAService
		rows.Scan(&ns.Name, &ns.ID, &ns.AvailRepImpact, &ns.AvailPrdImpact,
			&ns.AvailFinImpact, &ns.ConfiRepImpact, &ns.ConfiPrdImpact,
			&ns.ConfiFinImpact, &ns.IntegRepImpact, &ns.IntegPrdImpact,
			&ns.IntegFinImpact,
			&ns.AvailRepProb, &ns.AvailPrdProb, &ns.AvailFinProb,
			&ns.ConfiRepProb, &ns.ConfiPrdProb, &ns.ConfiFinProb,
			&ns.IntegRepProb, &ns.IntegPrdProb, &ns.IntegFinProb,
			&ns.DefData)
		s.Services = append(s.Services, ns)
	}
	return nil
}

func mergeSystemGroups(op opContext, s *slib.Service, groups []slib.SystemGroup) error {
	if len(groups) == 0 {
		return nil
	}
	s.Found = true
	s.SystemGroup = groups[0]
	err := serviceLookup(op, s)
	if err != nil {
		return err
	}
	return nil
}

func noteDynamicHost(op opContext, hn string, confidence int) error {
	// Don't add the host if it already exists in the static table.
	rows, err := op.Query(`SELECT hostid, dynamic FROM host
		WHERE lower(hostname) = lower($1)`, hn)
	if err != nil {
		return err
	}
	if rows.Next() {
		rows.Close()
		return nil
	}
	comment := fmt.Sprintf("dynamic entry for %v", hn)
	_, err = op.Exec(`INSERT INTO host
		(hostname, comment, dynamic,
		dynamic_confidence, dynamic_added, lastused)
		VALUES ($1, $2, true, $3, now() AT TIME ZONE 'utc',
		now() AT TIME ZONE 'utc')`,
		hn, comment, confidence)
	if err != nil {
		return err
	}
	return nil
}

func updateLastUsedHost(op opContext, hn string) error {
	_, err := op.Exec(`UPDATE host
		SET lastused = now() AT TIME ZONE 'utc'
		WHERE lower(hostname) = lower($1)`, hn)
	if err != nil {
		return err
	}
	return nil
}

func searchUsingHost(op opContext, hn string) (slib.Service, error) {
	var ret slib.Service
	err := updateLastUsedHost(op, hn)
	if err != nil {
		return ret, err
	}
	rows, err := op.Query(`SELECT sysgroupid, name, environment
		FROM sysgroup WHERE sysgroupid IN (
		SELECT DISTINCT sysgroupid
		FROM host WHERE hostname = $1 )`, hn)
	if err != nil {
		return ret, err
	}
	groups := make([]slib.SystemGroup, 0)
	for rows.Next() {
		var n slib.SystemGroup
		err = rows.Scan(&n.ID, &n.Name, &n.Environment)
		groups = append(groups, n)
	}
	err = mergeSystemGroups(op, &ret, groups)
	if err != nil {
		return ret, err
	}
	// If we successfully matched on hostname, also add any extended
	// information about this particular host to the result.
	if ret.Found {
		var tcw sql.NullBool
		var techowner sql.NullString
		rows, err = op.Query(`SELECT requiretcw, techowner
			FROM host
			LEFT OUTER JOIN techowners
			ON host.techownerid = techowners.techownerid
			WHERE hostname = $1`, hn)
		if err != nil {
			return ret, err
		}
		if rows.Next() {
			err = rows.Scan(&tcw, &techowner)
			if tcw.Valid {
				ret.TCW = tcw.Bool
			}
			if techowner.Valid {
				ret.TechOwner = techowner.String
			}
			rows.Close()
		}
	}
	return ret, nil
}

func searchUsingHostMatch(op opContext, hn string) (slib.Service, error) {
	var ret slib.Service

	// Use hostmatch to see if we can identify the system group.
	rows, err := op.Query(`SELECT sysgroupid, name, environment
		FROM sysgroup WHERE sysgroupid IN (
		SELECT DISTINCT sysgroupid
		FROM hostmatch WHERE
		$1 ~* expression )`, hn)
	if err != nil {
		return ret, err
	}
	groups := make([]slib.SystemGroup, 0)
	for rows.Next() {
		var n slib.SystemGroup
		err = rows.Scan(&n.ID, &n.Name, &n.Environment)
		groups = append(groups, n)
	}
	err = mergeSystemGroups(op, &ret, groups)
	if err != nil {
		return ret, err
	}

	return ret, nil
}

func searchHost(op opContext, hn string, conf int) (slib.Service, error) {
	hn = strings.ToLower(hn)
	sr, err := searchUsingHost(op, hn)
	if err != nil || sr.Found {
		return sr, err
	}
	if conf > 50 {
		err = noteDynamicHost(op, hn, conf)
	}
	sr, err = searchUsingHostMatch(op, hn)
	if err != nil || sr.Found {
		return sr, err
	}
	return sr, nil
}

func runSearch(o opContext, s slib.Search) error {
	var sres slib.Service
	var err error
	if s.Host != "" {
		sres, err = searchHost(o, s.Host, s.Confidence)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("a search did not specify any criteria")
	}
	sresstr, err := json.Marshal(&sres)
	if err != nil {
		return err
	}
	_, err = o.Exec(`INSERT INTO searchresults
		VALUES ( $1, $2, $3, now() AT TIME ZONE 'utc')`, o.opid, s.Identifier, string(sresstr))
	if err != nil {
		return err
	}
	return nil
}

// Process a new service lookup request.
func serviceNewSearch(rw http.ResponseWriter, req *http.Request) {
	req.ParseMultipartForm(10000000)

	val := req.FormValue("params")
	if val == "" {
		logf("no search criteria specified")
		http.Error(rw, "no search criteria specified", 500)
		return
	}
	var params slib.SearchParams
	err := json.Unmarshal([]byte(val), &params)
	if err != nil {
		logf(err.Error())
		http.Error(rw, err.Error(), 500)
		return
	}

	op := opContext{}
	op.newContext(dbconn, true, req.RemoteAddr)

	for _, x := range params.Searches {
		err = runSearch(op, x)
		if err != nil {
			op.logf(err.Error())
			http.Error(rw, err.Error(), 500)
			err = op.rollback()
			if err != nil {
				panic(err)
			}
			return
		}
	}
	op.commit()

	sr := slib.SearchResponse{SearchID: op.opid}
	buf, err := json.Marshal(&sr)
	if err != nil {
		panic(err)
	}
	fmt.Fprint(rw, string(buf))
}

// Purge used search results from the database.
func purgeSearchResult(op opContext, sid string) error {
	_, err := op.Exec(`DELETE FROM searchresults
		WHERE opid = $1`, sid)
	if err != nil {
		return err
	}
	return nil
}

// Given a search ID, respond with any results.
func serviceGetSearchID(rw http.ResponseWriter, req *http.Request) {
	req.ParseForm()

	op := opContext{}
	op.newContext(dbconn, false, req.RemoteAddr)

	sid := req.FormValue("id")
	if sid == "" {
		op.logf("must specify a valid search id")
		http.Error(rw, "must specify a valid search id", 500)
		return
	}

	rows, err := op.Query(`SELECT identifier, result
		FROM searchresults WHERE opid = $1`, sid)
	if err != nil {
		op.logf(err.Error())
		http.Error(rw, err.Error(), 500)
		return
	}
	sidr := slib.SearchIDResponse{}
	sidr.Results = make([]slib.SearchResult, 0)
	for rows.Next() {
		var resstr string
		var s slib.Service
		nr := slib.SearchResult{}
		err = rows.Scan(&nr.Identifier, &resstr)
		if err != nil {
			op.logf(err.Error())
			http.Error(rw, err.Error(), 500)
			return
		}
		err = json.Unmarshal([]byte(resstr), &s)
		if err != nil {
			op.logf(err.Error())
			http.Error(rw, err.Error(), 500)
			return
		}
		nr.Service = s
		sidr.Results = append(sidr.Results, nr)
	}

	buf, err := json.Marshal(&sidr)
	if err != nil {
		op.logf(err.Error())
		http.Error(rw, err.Error(), 500)
		return
	}

	err = purgeSearchResult(op, sid)
	if err != nil {
		op.logf(err.Error())
		http.Error(rw, err.Error(), 500)
		return
	}

	fmt.Fprint(rw, string(buf))
}

// Search for any hosts that contain a given substring.
func serviceSearchMatch(rw http.ResponseWriter, req *http.Request) {
	op := opContext{}
	op.newContext(dbconn, false, req.RemoteAddr)

	hm := req.FormValue("hostname")
	if hm == "" {
		http.Error(rw, "no search criteria specified", 500)
		return
	}
	hm = "%" + hm + "%"
	rows, err := op.Query(`SELECT hostid, hostname, sysgroupid,
		dynamic FROM host WHERE hostname ILIKE $1`, hm)
	if err != nil {
		op.logf(err.Error())
		http.Error(rw, err.Error(), 500)
		return
	}
	resp := slib.SearchMatchResponse{}
	for rows.Next() {
		hn := slib.Host{}
		var sgid sql.NullInt64
		var dynamic bool
		err = rows.Scan(&hn.ID, &hn.Hostname, &sgid, &dynamic)
		if err != nil {
			op.logf(err.Error())
			http.Error(rw, err.Error(), 500)
			return
		}
		if sgid.Valid {
			hn.SysGroupID = int(sgid.Int64)
		} else if !sgid.Valid && dynamic {
			tmpid, err := hostDynSysgroup(op, hn.Hostname)
			if err != nil {
				op.logf(err.Error())
				http.Error(rw, err.Error(), 500)
				return
			}
			hn.SysGroupID = tmpid
		}
		resp.Hosts = append(resp.Hosts, hn)
	}

	buf, err := json.Marshal(&resp)
	if err != nil {
		op.logf(err.Error())
		http.Error(rw, err.Error(), 500)
		return
	}
	fmt.Fprintf(rw, string(buf))
}

// Periodically prune dynamic hosts from the database that have not been seen
// within a given period.
func dynHostManager() {
	defer func() {
		if e := recover(); e != nil {
			logf("error in dynamic host manager: %v", e)
		}
	}()
	op := opContext{}
	op.newContext(dbconn, false, "dynhostmanager")
	cutoff := time.Now().UTC().Add(-(168 * time.Hour))
	rows, err := op.Query(`SELECT hostid FROM host
			WHERE dynamic = true AND lastused < $1`, cutoff)
	if err != nil {
		panic(err)
	}
	for rows.Next() {
		var hostid int
		err = rows.Scan(&hostid)
		if err != nil {
			panic(err)
		}
		_, err = op.Exec(`DELETE FROM compscore
				WHERE hostid = $1`, hostid)
		if err != nil {
			panic(err)
		}
		_, err = op.Exec(`DELETE FROM host
				WHERE hostid = $1`, hostid)
		if err != nil {
			panic(err)
		}
	}
}

func dbInit() error {
	var err error
	connstr := fmt.Sprintf("dbname=%v host=%v", cfg.Database.Database, cfg.Database.Hostname)
	dbconn, err = sql.Open("postgres", connstr)
	if err != nil {
		return err
	}
	return nil
}

func logf(s string, args ...interface{}) {
	buf := fmt.Sprintf(s, args...)
	tstr := time.Now().Format("2006-01-02 15:04:05")
	logbuf := fmt.Sprintf("[%v] %v", tstr, buf)
	logChan <- logbuf
}

func doExit(r int) {
	close(logChan)
	wg.Wait()
	os.Remove(pidFile)
	os.Exit(r)
}

func createPid() error {
	var err error
	pidFD, err = os.OpenFile(pidFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	fmt.Fprintf(pidFD, "%v", os.Getpid())
	pidFD.Close()
	return nil
}

func main() {
	var cfgpath string

	flag.StringVar(&cfgpath, "f", "", "path to configuration file")
	flag.StringVar(&pidFile, "p", "/var/run/serviceapi.pid", "path to pid file")
	flag.Parse()

	if cfgpath == "" {
		fmt.Fprintf(os.Stderr, "error: must specify configuration file with -f\n")
		os.Exit(1)
	}

	err := gcfg.ReadFileInto(&cfg, cfgpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	err = cfg.validate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	err = dbInit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigch
		doExit(0)
	}()

	logChan = make(chan string, 64)
	wg.Add(1)
	go func() {
		for x := range logChan {
			fmt.Fprintf(os.Stdout, "%v\n", x)
		}
		wg.Done()
	}()

	err = createPid()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Spawn the compliance scoring process
	go func() {
		logf("spawning compliance scoring routine")
		for {
			scoreCompliance()
			time.Sleep(5 * time.Second)
		}
	}()
	// Spawn dynamic host manager
	go func() {
		logf("spawning dynamic host manager")
		for {
			time.Sleep(1 * time.Minute)
			dynHostManager()
		}
	}()

	logf("Starting processing")

	go dynHostManager()

	r := mux.NewRouter()
	s := r.PathPrefix("/api/v1").Subrouter()
	s.HandleFunc("/search", serviceNewSearch).Methods("POST")
	s.HandleFunc("/search/results/id", serviceGetSearchID).Methods("GET")
	s.HandleFunc("/search/match", serviceSearchMatch).Methods("GET")
	s.HandleFunc("/sysgroups", serviceSysGroups).Methods("GET")
	s.HandleFunc("/sysgroup/id", serviceGetSysGroup).Methods("GET")
	s.HandleFunc("/rras", serviceRRAs).Methods("GET")
	s.HandleFunc("/risks", serviceRisks).Methods("GET")
	s.HandleFunc("/rra/id", serviceGetRRA).Methods("GET")
	s.HandleFunc("/rra/risk", serviceGetRRARisk).Methods("GET")
	s.HandleFunc("/vulns/target", serviceGetVulnsTarget).Methods("GET")
	http.Handle("/", context.ClearHandler(r))
	listenAddr := cfg.General.Listen
	err = http.ListenAndServeTLS(listenAddr, cfg.General.Cert, cfg.General.Key, nil)
	if err != nil {
		logf("http.ListenAndServeTLS: %v", err)
		doExit(1)
	}

	doExit(0)
}
