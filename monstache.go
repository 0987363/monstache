package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	elastigo "github.com/mattbaird/elastigo/lib"
	"github.com/robertkrimen/otto"
	_ "github.com/robertkrimen/otto/underscore"
	"github.com/rwynn/gtm"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var infoLog *log.Logger = log.New(os.Stdout, "INFO ", log.Flags())

var mapEnvs map[string]*executionEnv
var mapIndexTypes map[string]*indexTypeMapping
var fileNamespaces map[string]bool

var chunksRegex = regexp.MustCompile("\\.chunks$")
var systemsRegex = regexp.MustCompile("system\\..+$")

const mongoUrlDefault string = "localhost"
const resumeNameDefault string = "default"
const elasticMaxConnsDefault int = 10
const gtmChannelSizeDefault int = 100

type executionEnv struct {
	Vm     *otto.Otto
	Script string
}

type javascript struct {
	Namespace string
	Script    string
}

type indexTypeMapping struct {
	Namespace string
	Index     string
	Type      string
}

type configOptions struct {
	MongoUrl            string `toml:"mongo-url"`
	MongoPemFile        string `toml:"mongo-pem-file"`
	ElasticUrl          string `toml:"elasticsearch-url"`
	ElasticPemFile      string `toml:"elasticsearch-pem-file"`
	ResumeName          string `toml:"resume-name"`
	NsRegex             string `toml:"namespace-regex"`
	NsExcludeRegex      string `toml:"namespace-exclude-regex"`
	Verbose             bool
	Resume              bool
	Replay              bool
	DroppedDatabases    bool `toml:"dropped-databases"`
	DroppedCollections  bool `toml:"dropped-collections"`
	IndexFiles          bool `toml:"index-files"`
	FileHighlighting    bool `toml:"file-highlighting"`
	ElasticMaxConns     int  `toml:"elasticsearch-max-conns"`
	ElasticRetrySeconds int  `toml:"elasticsearch-retry-seconds"`
	ElasticMaxDocs      int  `toml:"elasticsearch-max-docs"`
	ElasticMaxBytes     int  `toml:"elasticsearch-max-bytes"`
	ElasticMaxSeconds   int  `toml:"elasticsearch-max-seconds"`
	ElasticMajorVersion int
	ChannelSize         int   `toml:"gtm-channel-size"`
	MaxFileSize         int64 `toml:"max-file-size"`
	ConfigFile          string
	Script              []javascript
	Mapping             []indexTypeMapping
	FileNamespaces      []string `toml:"file-namespaces"`
}

func TestElasticSearchConn(conn *elastigo.Conn, configuration *configOptions) (err error) {
	var result map[string]interface{}
	body, err := conn.DoCommand("GET", "/", nil, nil)
	if err != nil {
		return
	}
	err = json.Unmarshal(body, &result)
	if err == nil {
		version := result["version"].(map[string]interface{})
		if version == nil {
			err = errors.New("Unable to determine elasticsearch version")
		} else {
			number := version["number"].(string)
			if number == "" {
				err = errors.New("Unable to determine elasticsearch version")
			} else if configuration.Verbose {
				infoLog.Printf("Successfully connected to elasticsearch version %s", number)
			}
			versionParts := strings.Split(number, ".")
			if len(versionParts) > 0 {
				version, err := strconv.Atoi(versionParts[0])
				if err == nil {
					configuration.ElasticMajorVersion = version
				}

			} else {
				err = errors.New("Unable to parse elasticsearch version")
			}
		}
	}
	return
}

func NormalizeIndexName(name string) (normal string) {
	normal = strings.ToLower(strings.TrimPrefix(name, "_"))
	return
}

func NormalizeTypeName(name string) (normal string) {
	normal = strings.TrimPrefix(name, "_")
	return
}

func NormalizeEsId(id string) (normal string) {
	normal = strings.TrimPrefix(id, "_")
	return
}

func DeleteIndexes(conn *elastigo.Conn, db string, configuration *configOptions) (err error) {
	for ns, m := range mapIndexTypes {
		parts := strings.SplitN(ns, ".", 2)
		if parts[0] == db {
			if _, err = conn.DeleteIndex(m.Index + "*"); err != nil {
				return
			}
		}
	}
	_, err = conn.DeleteIndex(NormalizeIndexName(db) + "*")
	return
}

func DeleteIndex(conn *elastigo.Conn, namespace string, configuration *configOptions) (err error) {
	esIndex := NormalizeIndexName(namespace)
	if m := mapIndexTypes[namespace]; m != nil {
		esIndex = m.Index
	}
	_, err = conn.DeleteIndex(esIndex)
	return err
}

func IngestAttachment(conn *elastigo.Conn, esIndex string, esType string, esId string, data map[string]interface{}) (err error) {
	var body []byte
	args := map[string]interface{}{
		"pipeline": "attachment",
	}
	body, err = json.Marshal(data)
	if err == nil {
		_, err = conn.DoCommand("PUT", fmt.Sprintf("/%s/%s/%s", esIndex, esType, esId), args, string(body))
	}
	return err
}

func EnsureFileMapping(conn *elastigo.Conn, namespace string, configuration *configOptions) (err error) {
	if configuration.ElasticMajorVersion < 5 {
		return EnsureFileMappingMapperAttachment(conn, namespace, configuration)
	} else {
		return EnsureFileMappingIngestAttachment(conn, namespace, configuration)
	}
}

func EnsureFileMappingIngestAttachment(conn *elastigo.Conn, namespace string, configuration *configOptions) (err error) {
	var body []byte
	pipeline := map[string]interface{}{
		"description": "Extract file information",
		"processors": [1]map[string]interface{}{
			map[string]interface{}{
				"attachment": map[string]interface{}{
					"field": "file",
				},
			},
		},
	}
	body, err = json.Marshal(pipeline)
	if err == nil {
		_, err = conn.DoCommand("PUT", "/_ingest/pipeline/attachment", nil, string(body))
	}
	return err
}

func EnsureFileMappingMapperAttachment(conn *elastigo.Conn, namespace string, configuration *configOptions) (err error) {
	var body []byte
	parts := strings.SplitN(namespace, ".", 2)
	esIndex, esType := NormalizeIndexName(namespace), NormalizeTypeName(parts[1])
	if m := mapIndexTypes[namespace]; m != nil {
		esIndex, esType = m.Index, m.Type
	}
	props := map[string]interface{}{
		"properties": map[string]interface{}{
			"file": map[string]interface{}{
				"type": "attachment",
			},
		},
	}
	file := props["properties"].(map[string]interface{})["file"].(map[string]interface{})
	types := map[string]interface{}{
		esType: props,
	}
	mappings := map[string]interface{}{
		"mappings": types,
	}
	if configuration.FileHighlighting {
		file["fields"] = map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"term_vector": "with_positions_offsets",
				"store":       true,
			},
		}
	}
	if exists, _ := conn.ExistsIndex(esIndex, "", nil); exists {
		body, err = json.Marshal(types)
		if err != nil {
			return err
		}
		_, err = conn.DoCommand("PUT", fmt.Sprintf("/%s/%s/_mapping", esIndex, esType), nil, string(body))
	} else {
		body, err = json.Marshal(mappings)
		if err != nil {
			return err
		}
		_, err = conn.DoCommand("PUT", fmt.Sprintf("/%s", esIndex), nil, string(body))
	}
	return err
}

func DefaultIndexTypeMapping(op *gtm.Op) *indexTypeMapping {
	return &indexTypeMapping{
		Namespace: op.Namespace,
		Index:     NormalizeIndexName(op.Namespace),
		Type:      NormalizeTypeName(op.GetCollection()),
	}
}

func IndexTypeMapping(op *gtm.Op) *indexTypeMapping {
	mapping := DefaultIndexTypeMapping(op)
	if mapIndexTypes != nil {
		if m := mapIndexTypes[op.Namespace]; m != nil {
			mapping = m
		}
	}
	return mapping
}

func OpIdToString(op *gtm.Op) string {
	var opIdStr string
	switch op.Id.(type) {
	case bson.ObjectId:
		opIdStr = op.Id.(bson.ObjectId).Hex()
	default:
		opIdStr = NormalizeEsId(fmt.Sprintf("%v", op.Id))
	}
	return opIdStr
}

func MapData(op *gtm.Op) error {
	if mapEnvs == nil {
		return nil
	}
	if env := mapEnvs[op.Namespace]; env != nil {
		val, err := env.Vm.Call("module.exports", op.Data, op.Data)
		if err != nil {
			return err
		} else if !val.IsObject() {
			return errors.New("exported function must return an object")
		}
		data, err := val.Export()
		if err != nil {
			return err
		} else if data == val {
			return errors.New("exported function must return an object")
		} else {
			op.Data = data.(map[string]interface{})
		}
	}
	return nil
}

func PrepareDataForIndexing(data map[string]interface{}) {
	delete(data, "_id")
	delete(data, "_type")
	delete(data, "_index")
	delete(data, "_score")
	delete(data, "_source")
}

func AddFileContent(session *mgo.Session, op *gtm.Op, configuration *configOptions) (err error) {
	var buff bytes.Buffer
	op.Data["file"] = ""
	writer, db, bucket :=
		bufio.NewWriter(&buff),
		session.DB(op.GetDatabase()),
		strings.SplitN(op.GetCollection(), ".", 2)[0]
	encoder := base64.NewEncoder(base64.StdEncoding, writer)
	file, err := db.GridFS(bucket).OpenId(op.Data["_id"])
	if err != nil {
		return
	}
	defer file.Close()
	if configuration.MaxFileSize > 0 {
		if file.Size() > configuration.MaxFileSize {
			infoLog.Printf("file %s md5(%s) exceeds max file size. file content omitted.",
				file.Name(), file.MD5())
			return
		}
	}
	if _, err = io.Copy(encoder, file); err != nil {
		return
	}
	encoder.Close()
	writer.Flush()
	op.Data["file"] = string(buff.Bytes())
	return
}

func NotMonstache(op *gtm.Op) bool {
	return op.GetDatabase() != "monstache"
}

func NotChunks(op *gtm.Op) bool {
	return !chunksRegex.MatchString(op.GetCollection())
}

func NotSystem(op *gtm.Op) bool {
	return !systemsRegex.MatchString(op.GetCollection())
}

func FilterWithRegex(regex string) gtm.OpFilter {
	var validNameSpace = regexp.MustCompile(regex)
	return func(op *gtm.Op) bool {
		return validNameSpace.MatchString(op.Namespace)
	}
}

func FilterInverseWithRegex(regex string) gtm.OpFilter {
	var invalidNameSpace = regexp.MustCompile(regex)
	return func(op *gtm.Op) bool {
		return !invalidNameSpace.MatchString(op.Namespace)
	}
}

func SaveTimestamp(session *mgo.Session, op *gtm.Op, resumeName string) error {
	col := session.DB("monstache").C("monstache")
	doc := make(map[string]interface{})
	doc["ts"] = op.Timestamp
	_, err := col.UpsertId(resumeName, bson.M{"$set": doc})
	return err
}

func (configuration *configOptions) ParseCommandLineFlags() *configOptions {
	flag.StringVar(&configuration.MongoUrl, "mongo-url", "", "MongoDB connection URL")
	flag.StringVar(&configuration.MongoPemFile, "mongo-pem-file", "", "Path to a PEM file for secure connections to MongoDB")
	flag.StringVar(&configuration.ElasticUrl, "elasticsearch-url", "", "ElasticSearch connection URL")
	flag.StringVar(&configuration.ElasticPemFile, "elasticsearch-pem-file", "", "Path to a PEM file for secure connections to elasticsearch")
	flag.IntVar(&configuration.ElasticMaxConns, "elasticsearch-max-conns", 0, "ElasticSearch max connections")
	flag.IntVar(&configuration.ElasticRetrySeconds, "elasticsearch-retry-seconds", 0, "Number of seconds before retrying ElasticSearch requests")
	flag.IntVar(&configuration.ElasticMaxDocs, "elasticsearch-max-docs", 0, "Number of docs to hold before flushing to ElasticSearch")
	flag.IntVar(&configuration.ElasticMaxBytes, "elasticsearch-max-bytes", 0, "Number of bytes to hold before flushing to ElasticSearch")
	flag.IntVar(&configuration.ElasticMaxSeconds, "elasticsearch-max-seconds", 0, "Number of seconds before flushing to ElasticSearch")
	flag.IntVar(&configuration.ChannelSize, "gtm-channel-size", 0, "Size of gtm channels")
	flag.Int64Var(&configuration.MaxFileSize, "max-file-size", 0, "GridFs file content exceeding this limit in bytes will not be indexed in ElasticSearch")
	flag.StringVar(&configuration.ConfigFile, "f", "", "Location of configuration file")
	flag.BoolVar(&configuration.DroppedDatabases, "dropped-databases", true, "True to delete indexes from dropped databases")
	flag.BoolVar(&configuration.DroppedCollections, "dropped-collections", true, "True to delete indexes from dropped collections")
	flag.BoolVar(&configuration.Verbose, "verbose", false, "True to output verbose messages")
	flag.BoolVar(&configuration.Resume, "resume", false, "True to capture the last timestamp of this run and resume on a subsequent run")
	flag.BoolVar(&configuration.Replay, "replay", false, "True to replay all events from the oplog and index them in elasticsearch")
	flag.BoolVar(&configuration.IndexFiles, "index-files", false, "True to index gridfs files into elasticsearch. Requires the elasticsearch mapper-attachments (deprecated) or ingest-attachment plugin")
	flag.BoolVar(&configuration.FileHighlighting, "file-highlighting", false, "True to enable the ability to highlight search times for a file query")
	flag.StringVar(&configuration.ResumeName, "resume-name", "", "Name under which to load/store the resume state. Defaults to 'default'")
	flag.StringVar(&configuration.NsRegex, "namespace-regex", "", "A regex which is matched against an operation's namespace (<database>.<collection>).  Only operations which match are synched to elasticsearch")
	flag.StringVar(&configuration.NsRegex, "namespace-exclude-regex", "", "A regex which is matched against an operation's namespace (<database>.<collection>).  Only operations which do not match are synched to elasticsearch")
	flag.Parse()
	return configuration
}

func (configuration *configOptions) LoadIndexTypes() {
	if configuration.Mapping != nil {
		mapIndexTypes = make(map[string]*indexTypeMapping)
		for _, m := range configuration.Mapping {
			if m.Namespace != "" && m.Index != "" && m.Type != "" {
				mapIndexTypes[m.Namespace] = &indexTypeMapping{
					Namespace: m.Namespace,
					Index:     NormalizeIndexName(m.Index),
					Type:      NormalizeTypeName(m.Type),
				}
			} else {
				panic("mappings must specify namespace, index, and type attributes")
			}
		}
	}
}

func (configuration *configOptions) LoadScripts() {
	if configuration.Script != nil {
		mapEnvs = make(map[string]*executionEnv)
		for _, s := range configuration.Script {
			if s.Namespace != "" && s.Script != "" {
				env := &executionEnv{
					Vm:     otto.New(),
					Script: s.Script,
				}
				if err := env.Vm.Set("module", make(map[string]interface{})); err != nil {
					panic(err)
				}
				if _, err := env.Vm.Run(env.Script); err != nil {
					panic(err)
				}
				val, err := env.Vm.Run("module.exports")
				if err != nil {
					panic(err)
				} else if !val.IsFunction() {
					panic("module.exports must be a function")

				}
				mapEnvs[s.Namespace] = env
			} else {
				panic("scripts must specify namespace and script attributes")
			}
		}
	}
}

func (configuration *configOptions) LoadConfigFile() *configOptions {
	if configuration.ConfigFile != "" {
		var tomlConfig configOptions = configOptions{
			DroppedDatabases:   true,
			DroppedCollections: true,
		}
		if _, err := toml.DecodeFile(configuration.ConfigFile, &tomlConfig); err != nil {
			panic(err)
		}
		if configuration.MongoUrl == "" {
			configuration.MongoUrl = tomlConfig.MongoUrl
		}
		if configuration.MongoPemFile == "" {
			configuration.MongoPemFile = tomlConfig.MongoPemFile
		}
		if configuration.ElasticPemFile == "" {
			configuration.ElasticPemFile = tomlConfig.ElasticPemFile
		}
		if configuration.ElasticUrl == "" {
			configuration.ElasticUrl = tomlConfig.ElasticUrl
		}
		if configuration.ElasticMaxConns == 0 {
			configuration.ElasticMaxConns = tomlConfig.ElasticMaxConns
		}
		if configuration.ElasticRetrySeconds == 0 {
			configuration.ElasticRetrySeconds = tomlConfig.ElasticRetrySeconds
		}
		if configuration.ElasticMaxDocs == 0 {
			configuration.ElasticMaxDocs = tomlConfig.ElasticMaxDocs
		}
		if configuration.ElasticMaxBytes == 0 {
			configuration.ElasticMaxBytes = tomlConfig.ElasticMaxBytes
		}
		if configuration.ElasticMaxSeconds == 0 {
			configuration.ElasticMaxSeconds = tomlConfig.ElasticMaxSeconds
		}
		if configuration.ChannelSize == 0 {
			configuration.ChannelSize = tomlConfig.ChannelSize
		}
		if configuration.MaxFileSize == 0 {
			configuration.MaxFileSize = tomlConfig.MaxFileSize
		}
		if configuration.DroppedDatabases && !tomlConfig.DroppedDatabases {
			configuration.DroppedDatabases = false
		}
		if configuration.DroppedCollections && !tomlConfig.DroppedCollections {
			configuration.DroppedCollections = false
		}
		if !configuration.Verbose && tomlConfig.Verbose {
			configuration.Verbose = true
		}
		if !configuration.IndexFiles && tomlConfig.IndexFiles {
			configuration.IndexFiles = true
		}
		if !configuration.FileHighlighting && tomlConfig.FileHighlighting {
			configuration.FileHighlighting = true
		}
		if !configuration.Replay && tomlConfig.Replay {
			configuration.Replay = true
		}
		if !configuration.Resume && tomlConfig.Resume {
			configuration.Resume = true
		}
		if configuration.Resume && configuration.ResumeName == "" {
			configuration.ResumeName = tomlConfig.ResumeName
		}
		if configuration.NsRegex == "" {
			configuration.NsRegex = tomlConfig.NsRegex
		}
		if configuration.NsExcludeRegex == "" {
			configuration.NsExcludeRegex = tomlConfig.NsExcludeRegex
		}
		if configuration.IndexFiles {
			configuration.FileNamespaces = tomlConfig.FileNamespaces
			tomlConfig.LoadGridFsConfig()
		}
		tomlConfig.LoadScripts()
		tomlConfig.LoadIndexTypes()
	}
	return configuration
}

func (configuration *configOptions) LoadGridFsConfig() *configOptions {
	fileNamespaces = make(map[string]bool)
	for _, namespace := range configuration.FileNamespaces {
		fileNamespaces[namespace] = true
	}
	return configuration
}

func (configuration *configOptions) SetDefaults() *configOptions {
	if configuration.MongoUrl == "" {
		configuration.MongoUrl = mongoUrlDefault
	}
	if configuration.ResumeName == "" {
		configuration.ResumeName = resumeNameDefault
	}
	if configuration.ElasticMaxConns == 0 {
		configuration.ElasticMaxConns = elasticMaxConnsDefault
	}
	if configuration.ChannelSize == 0 {
		configuration.ChannelSize = gtmChannelSizeDefault
	}
	return configuration
}

func (configuration *configOptions) DialMongo() (*mgo.Session, error) {
	if configuration.MongoPemFile != "" {
		certs := x509.NewCertPool()
		if ca, err := ioutil.ReadFile(configuration.MongoPemFile); err == nil {
			certs.AppendCertsFromPEM(ca)
		} else {
			return nil, err
		}
		tlsConfig := &tls.Config{RootCAs: certs}
		dialInfo, err := mgo.ParseURL(configuration.MongoUrl)
		if err != nil {
			return nil, err
		} else {
			dialInfo.DialServer = func(addr *mgo.ServerAddr) (net.Conn, error) {
				return tls.Dial("tcp", addr.String(), tlsConfig)
			}
			return mgo.DialWithInfo(dialInfo)
		}
	} else {
		return mgo.Dial(configuration.MongoUrl)
	}
}

func (configuration *configOptions) ConfigHttpTransport() error {
	if configuration.ElasticPemFile != "" {
		certs := x509.NewCertPool()
		if ca, err := ioutil.ReadFile(configuration.ElasticPemFile); err == nil {
			certs.AppendCertsFromPEM(ca)
		} else {
			return err
		}
		tlsConfig := &tls.Config{RootCAs: certs}
		http.DefaultTransport.(*http.Transport).TLSClientConfig = tlsConfig
	}
	return nil
}

func TraceRequest(method, url, body string) {
	infoLog.Printf("%s request sent to %s", method, url)
	if body != "" {
		infoLog.Printf("request body: %s", body)
	}
}

func main() {
	log.SetPrefix("ERROR ")

	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	configuration := &configOptions{}
	configuration.ParseCommandLineFlags().LoadConfigFile().SetDefaults()

	if err := configuration.ConfigHttpTransport(); err != nil {
		log.Panicf("Unable to configure HTTP transport: %s", err)
	}
	mongo, err := configuration.DialMongo()
	if err != nil {
		log.Panicf("Unable to connect to mongodb using URL %s: %s", configuration.MongoUrl, err)
	}
	defer mongo.Close()
	mongo.SetMode(mgo.Monotonic, true)

	elastic := elastigo.NewConn()
	if configuration.ElasticUrl != "" {
		elastic.SetFromUrl(configuration.ElasticUrl)
	}
	if configuration.Verbose {
		elastic.RequestTracer = TraceRequest
	}
	if err := TestElasticSearchConn(elastic, configuration); err != nil {
		log.Panicf("Unable to validate connection to elasticsearch using %s://%s:%s: %s",
			elastic.Protocol, elastic.Domain, elastic.Port, err)
	}
	indexer := elastic.NewBulkIndexerErrors(configuration.ElasticMaxConns, configuration.ElasticRetrySeconds)
	if configuration.ElasticMaxDocs != 0 {
		indexer.BulkMaxDocs = configuration.ElasticMaxDocs
	}
	if configuration.ElasticMaxBytes != 0 {
		indexer.BulkMaxBuffer = configuration.ElasticMaxBytes
	}
	if configuration.ElasticMaxSeconds != 0 {
		indexer.BufferDelayMax = time.Duration(configuration.ElasticMaxSeconds) * time.Second
	}
	indexer.Start()
	defer indexer.Stop()

	go func(mongo *mgo.Session, indexer *elastigo.BulkIndexer) {
		<-sigs
		mongo.Close()
		indexer.Flush()
		indexer.Stop()
		done <- true
	}(mongo, indexer)

	var after gtm.TimestampGenerator = nil
	if configuration.Resume {
		after = func(session *mgo.Session, options *gtm.Options) bson.MongoTimestamp {
			ts := gtm.LastOpTimestamp(session, options)
			if configuration.Replay {
				ts = 0
			} else {
				collection := session.DB("monstache").C("monstache")
				doc := make(map[string]interface{})
				collection.FindId(configuration.ResumeName).One(doc)
				if doc["ts"] != nil {
					ts = doc["ts"].(bson.MongoTimestamp)
				}
			}
			return ts
		}
	} else if configuration.Replay {
		after = func(session *mgo.Session, options *gtm.Options) bson.MongoTimestamp {
			return 0
		}
	}

	if configuration.IndexFiles {
		if len(configuration.FileNamespaces) == 0 {
			log.Fatalln("File indexing is ON but no file namespaces are configured")
		}
		for _, namespace := range configuration.FileNamespaces {
			if err := EnsureFileMapping(elastic, namespace, configuration); err != nil {
				panic(err)
			}
			if configuration.ElasticMajorVersion >= 5 {
				break
			}
		}
	}

	var filter gtm.OpFilter = nil
	filterChain := []gtm.OpFilter{NotMonstache, NotSystem, NotChunks}
	if configuration.NsRegex != "" {
		filterChain = append(filterChain, FilterWithRegex(configuration.NsRegex))
	}
	if configuration.NsExcludeRegex != "" {
		filterChain = append(filterChain, FilterInverseWithRegex(configuration.NsExcludeRegex))
	}
	filter = gtm.ChainOpFilters(filterChain...)

	ops, errs := gtm.Tail(mongo, &gtm.Options{
		After:       after,
		Filter:      filter,
		ChannelSize: configuration.ChannelSize,
	})
	exitStatus := 0
	for {
		select {
		case <-done:
			os.Exit(exitStatus)
		case err = <-errs:
			exitStatus = 1
			log.Println(err)
		case indexErr := <-indexer.ErrorChannel:
			errs <- indexErr.Err
		case op := <-ops:
			ingestAttachment, indexed, objectId, indexType := false, false, OpIdToString(op), IndexTypeMapping(op)
			if op.IsDrop() {
				if db, drop := op.IsDropDatabase(); drop {
					if configuration.DroppedDatabases {
						if err := DeleteIndexes(elastic, db, configuration); err == nil {
							indexed = true
						} else {
							errs <- err
						}
					} else {
						indexed = true
					}
				} else if col, drop := op.IsDropCollection(); drop {
					if configuration.DroppedCollections {
						if err := DeleteIndex(elastic, op.GetDatabase()+"."+col, configuration); err == nil {
							indexed = true
						} else {
							errs <- err
						}
					} else {
						indexed = true
					}
				}
			} else if op.IsDelete() {
				indexer.Delete(indexType.Index, indexType.Type, objectId)
				indexed = true
			} else if op.Data != nil {
				if configuration.IndexFiles {
					if fileNamespaces[op.Namespace] {
						if err := AddFileContent(mongo, op, configuration); err != nil {
							errs <- err
						}
						if configuration.ElasticMajorVersion >= 5 {
							if op.Data["file"] != "" {
								ingestAttachment = true
							}
						}
					}
				}
				PrepareDataForIndexing(op.Data)
				if err := MapData(op); err == nil {
					if ingestAttachment {
						if err := IngestAttachment(elastic, indexType.Index, indexType.Type, objectId, op.Data); err == nil {
							indexed = true
						} else {
							errs <- err
						}

					} else {
						if err := indexer.Index(indexType.Index, indexType.Type, objectId, "", "", nil, op.Data); err == nil {
							indexed = true
						} else {
							errs <- err
						}
					}
				} else {
					errs <- err
				}
			}
			if configuration.Resume && indexed {
				if err := SaveTimestamp(mongo, op, configuration.ResumeName); err != nil {
					errs <- err
				}
			}
		}
	}
}
