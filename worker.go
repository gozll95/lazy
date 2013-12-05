package main

import (
	"bytes"
	"github.com/bitly/go-nsq"
	"github.com/dustin/go-probably"
	"github.com/garyburd/redigo/redis"
	"github.com/jbrukh/bayesian"
	"github.com/jeromer/syslogparser/rfc3164"
	"log"
	"regexp"
	"strings"
	"sync"
	"text/scanner"
	"time"
)

type Analyzer struct {
	*redis.Pool
	*nsq.Writer
	*probably.Sketch
	c            *bayesian.Classifier
	count        int
	trainTopic   string
	trainChannel string
	msgChannel   chan []string
	regexMap     map[string][]*regexp.Regexp
	auditTags    map[string]string
	sync.Mutex
}

func (m *Analyzer) HandleMessage(msg *nsq.Message) error {
	p := rfc3164.NewParser(msg.Body)
	if err := p.Parse(); err != nil {
		return nil
	}
	message := p.Dump()
	words := m.parseLog(message["content"].(string))
	t, ok := message["timestamp"].(time.Time)
	if !ok {
		log.Println("time error: ", message["timestamp"], msg.Body)
		return nil
	}
	con := m.Get()
	defer con.Close()
	m.Lock()
	_, likely, strict := m.c.LogScores(words)
	rg, ok := m.regexMap[message["tag"].(string)]
	if _, exist := m.auditTags[message["tag"].(string)]; exist {
		con.Do("ZADD", message["tag"], t.Unix(), msg.Body)
	}
	m.Unlock()
	if strict && likely > 0 {
		log.Println(words, likely)
		con.Do("ZADD", "err:"+message["tag"].(string), t.Unix(), msg.Body)
	}
	if !strict {
		m.Publish(m.trainTopic, msg.Body)
		log.Println("failed bayes", msg.Body)
	}
	if ok {
		for _, r := range rg {
			if r.MatchString(message["content"].(string)) {
				return nil
			}
		}
	}
	log.Println(msg.Body)
	return nil
}

func (m *Analyzer) parseLog(msg string) []string {
	r := bytes.NewReader([]byte(msg))
	var s scanner.Scanner
	s.Init(r)
	tok := s.Scan()
	m.count++
	var tokens []string
	for tok != scanner.EOF {
		if tok == scanner.Ident || tok == scanner.String || tok == scanner.RawString {
			v := strings.ToLower(s.TokenText())
			p := uint32(m.count) / m.ConservativeIncrement(v)
			if p < 100 {
				log.Println("ignore", v, p)
			} else {
				tokens = append(tokens, v)
				log.Println("not ignore", v)
			}
		}
		tok = s.Scan()
	}
	return tokens
}

func (m *Analyzer) syncBayes() {
	ticker := time.Tick(time.Second * 600)
	for {
		<-ticker
		m.getBayes()
	}
}

func (m *Analyzer) syncRegexp() {
	ticker := time.Tick(time.Second * 600)
	for {
		<-ticker
		m.getRegexp()
	}
}

func (m *Analyzer) syncAuditTags() {
	ticker := time.Tick(time.Second * 3600)
	for {
		<-ticker
		m.getAuditTags()
	}
}

func (m *Analyzer) getBayes() {
	var NormalLog = bayesian.Class("NormalLog")
	var ErrorLog = bayesian.Class("ErrorLog")
	con := m.Get()
	defer con.Close()
	n, _ := redis.Strings(con.Do("SMEMBERS", "NormalLog"))
	e, _ := redis.Strings(con.Do("SMEMBERS", "ErrorLog"))
	m.Lock()
	defer m.Unlock()
	m.c = bayesian.NewClassifier(NormalLog, ErrorLog)
	m.c.Learn(n, NormalLog)
	m.c.Learn(e, ErrorLog)
}

func (m *Analyzer) getRegexp() {
	con := m.Get()
	defer con.Close()
	t, e := redis.Strings(con.Do("SMEMBERS", "logtags"))
	if e != nil {
		return
	}
	for _, value := range t {
		r, _ := redis.Strings(con.Do("SMEMBERS", "tag:"+value))
		var rg []*regexp.Regexp
		for _, v := range r {
			x, e := regexp.CompilePOSIX(v)
			if e != nil {
				log.Println(r, e)
				continue
			}
			rg = append(rg, x)
		}
		m.Lock()
		m.regexMap[value] = rg
		m.Unlock()
	}
}

func (m *Analyzer) getAuditTags() {
	con := m.Get()
	defer con.Close()
	t, e := redis.Strings(con.Do("SMEMBERS", "auditlogtags"))
	if e != nil {
		return
	}
	m.Lock()
	defer m.Unlock()
	for _, value := range t {
		m.auditTags[value] = value
	}
}

/*
type Learner struct {
	*redis.Pool
	probably.Sketch
	learnChannel chan []string
}

func (m *Learner) HandleMessage(msg *nsq.Message) error {
	return nil
}

func (m *Learner) Learn() {
	chaos := bayesian.Class("Chaos")
	sequence := bayesian.Class("Sequence")
	c := bayesian.NewClassifier(chaos, sequence)
	ticker := time.Tick(time.Second * 30)
	con := m.Get()
	defer con.Close()
	turn := 0
	for {
		select {
		case l := <-m.learnChannel:
		}
		turn++
	}
}
*/