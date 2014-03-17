package main

import (
	"encoding/json"
	"fmt"
	"github.com/bitly/go-nsq"
	"github.com/garyburd/redigo/redis"
	"github.com/jbrukh/bayesian"
	"github.com/mattbaird/elastigo/api"
	"github.com/mattbaird/elastigo/core"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

type LogParser struct {
	sync.Mutex
	*redis.Pool
	*Setting
	classifiers     []string
	logTopic        string
	logChannel      string
	reader          *nsq.Reader
	writer          *nsq.Writer
	logSetting      *LogSetting
	c               *bayesian.Classifier
	wordSplitRegexp *regexp.Regexp
	regexMap        map[string][]*regexp.Regexp
	exitChannel     chan int
	msgChannel      chan ElasticRecord
}

func (m *LogParser) Run() error {
	redisCon := func() (redis.Conn, error) {
		c, err := redis.Dial("tcp", m.RedisServer)
		if err != nil {
			return nil, err
		}
		return c, err
	}
	m.Pool = redis.NewPool(redisCon, 3)
	m.getLogFormat()
	m.wordSplitRegexp = regexp.MustCompile(m.logSetting.SplitRegexp)
	m.logChannel = "logtoelasticsearch"
	go m.elasticSearchBuildIndex()
	var err error
	m.reader, err = nsq.NewReader(m.logTopic, m.logChannel)
	if err != nil {
		log.Println(m.logTopic, err)
		return err
	}
	m.reader.SetMaxInFlight(m.MaxInFlight)
	for i := 0; i < m.MaxInFlight; i++ {
		m.reader.AddHandler(m)
	}
	for _, addr := range m.LookupdAddresses {
		err := m.reader.ConnectToLookupd(addr)
		if err != nil {
			return err
		}
	}
	go m.syncLogFormat()
	return err
}

func (m *LogParser) Stop() {
	m.reader.Stop()
	m.writer.Stop()
	close(m.exitChannel)
	m.Pool.Close()
}

func (m *LogParser) HandleMessage(msg *nsq.Message) error {
	body := make(map[string]string)
	err := json.Unmarshal(msg.Body, &body)
	if err != nil {
		return nil
	}
	record := ElasticRecord{
		errChannel: make(chan error),
		ttl:        m.logSetting.IndexTTL,
	}
	m.Lock()
	message, err := m.logSetting.Parser([]byte(body["rawmsg"]))
	if err != nil {
		log.Println(err)
		return nil
	}
	message["from"] = body["from"]
	if m.logSetting.LogType == "rfc3164" {
		tag := message["tag"].(string)
		if len(tag) == 0 {
			message["tag"] = "misc"
			tag = "misc"
		} else {
			tag = strings.Replace(tag, ".", "", -1)
		}
		for _, check := range m.logSetting.AddtionCheck {
			switch check {
			case "regexp":
				rg, ok := m.regexMap[tag]
				if ok {
					record.body["regexp_check"] = "failed"
					for _, r := range rg {
						if r.MatchString(message["content"].(string)) {
							record.body["regexp_check"] = "passed"
						}
					}
				}
			case "bayes":
				words := m.parseWords(message["content"].(string))
				_, likely, strict := m.c.LogScores(words)
				record.body["bayes_check"] = "chaos"
				if strict {
					record.body["bayes_check"] = m.classifiers[likely]
				} else {
					m.writer.Publish(m.TrainTopic, msg.Body)
				}
			default:
				log.Println("unsupportted check way", check)
			}
		}
	}
	m.Unlock()
	record.body = message
	m.msgChannel <- record
	return <-record.errChannel
}

func (m *LogParser) getLogFormat() {
	con := m.Get()
	defer con.Close()
	body, e := con.Do("GET", "logsetting:"+m.logTopic)
	if e != nil {
		return
	}
	m.Lock()
	defer m.Unlock()
	var logSetting LogSetting
	if err := json.Unmarshal(body.([]byte), &logSetting); err == nil {
		m.logSetting = &logSetting
	}

}

func (m *LogParser) getBayes() {
	con := m.Get()
	defer con.Close()
	classifiers, err := redis.Strings(con.Do("SMEMBERS", "classifiers:"+m.logTopic))
	if err != nil {
		log.Println("fail to get classifiers", classifiers)
		return
	}
	if len(classifiers) < 2 {
		log.Println("classifiers is less than 2")
		return
	}
	var classifierList []bayesian.Class
	for _, c := range classifiers {
		classifierList = append(classifierList, bayesian.Class(c))
	}
	m.Lock()
	defer m.Unlock()
	m.classifiers = classifiers
	m.c = bayesian.NewClassifier(classifierList...)
	for _, c := range classifierList {
		words, err := redis.Strings(con.Do("SMEMBERS", "classifier:"+m.logTopic+":"+string(c)))
		if err != nil {
			log.Println("fail to get words", c)
			return
		}
		m.c.Learn(words, c)
	}
}

func (m *LogParser) getRegexp() {
	con := m.Get()
	defer con.Close()
	t, e := redis.Strings(con.Do("SMEMBERS", "logtags"+m.logTopic))
	if e != nil {
		return
	}
	for _, value := range t {
		r, _ := redis.Strings(con.Do("SMEMBERS", "logtag:"+m.logTopic+":"+value))
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

func (m *LogParser) syncLogFormat() {
	ticker := time.Tick(time.Second * 600)
	for {
		select {
		case <-ticker:
			m.getLogFormat()
			for _, check := range m.logSetting.AddtionCheck {
				switch check {
				case "regexp":
					m.getRegexp()
				case "bayes":
					m.getBayes()
				default:
					log.Println("unsupportted check way", check)
				}
			}
		case <-m.exitChannel:
			return
		}
	}
}

func (m *LogParser) elasticSearchBuildIndex() {
	api.Domain = m.ElasticSearchHost
	api.Port = m.ElasticSearchPort
	indexor := core.NewBulkIndexorErrors(10, 60)
	done := make(chan bool)
	indexor.Run(done)
	var err error
	ticker := time.Tick(time.Second * 600)
	yy, mm, dd := time.Now().Date()
	indexPatten := fmt.Sprintf("-%d.%d.%d", yy, mm, dd)
	for {
		select {
		case <-ticker:
			yy, mm, dd = time.Now().Date()
			indexPatten = fmt.Sprintf("-%d.%d.%d", yy, mm, dd)
		case errBuf := <-indexor.ErrorChannel:
			log.Println(errBuf.Err)
		case r := <-m.msgChannel:
			m.Lock()
			searchIndex := m.logSetting.ElasticSearchIndex
			m.Unlock()
			err = indexor.Index(searchIndex+indexPatten, m.logTopic, "", r.ttl, nil, r.body)
			r.errChannel <- err
		case <-m.exitChannel:
			break
		}
	}
	done <- true
}

func (m *LogParser) parseWords(msg string) []string {
	t := strings.Split(m.wordSplitRegexp.ReplaceAllString(msg, " "), " ")
	var tokens []string
	for _, v := range t {
		tokens = append(tokens, strings.ToLower(v))
	}
	return tokens
}