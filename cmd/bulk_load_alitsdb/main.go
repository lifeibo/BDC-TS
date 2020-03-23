// The caller is responsible for assuring that the database is empty before
// bulk load.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	alitsdb_serialization "github.com/caict-benchmark/BDC-TS/alitsdb_serializaition"

	"github.com/caict-benchmark/BDC-TS/bulk_data_gen/vehicle"

	"github.com/caict-benchmark/BDC-TS/bulk_data_gen/common"
	"github.com/caict-benchmark/BDC-TS/util/report"
	"github.com/klauspost/compress/gzip"
	"github.com/pkg/profile"
)

// Program option vars:
var (
	hosts          string
	port           int
	useCase        string
	daemonUrls     []string
	workers        int
	batchSize      int
	backoff        time.Duration
	doLoad         bool
	memprofile     bool
	debug          bool
	cpuProfile     string
	viaHTTP        bool
	jsonFormat     bool
	reportDatabase string
	reportHost     string
	reportUser     string
	reportPassword string
	reportTagsCSV  string
)

// Global vars
var (
	bufPool sync.Pool

	// channel for http write
	batchChan chan *bytes.Buffer
	// channel for RPC write
	batchPointsChan chan []*alitsdb_serialization.MultifieldPoint

	monitorChan chan bool

	inputDone      chan struct{}
	workersGroup   sync.WaitGroup
	backingOffChan chan bool
	backingOffDone chan struct{}
	reportTags     [][2]string
	reportHostname string
	FieldsNum      int

	openbracket  = []byte("[")
	closebracket = []byte("]")
	commaspace   = []byte(", ")
	newline      = []byte("\n")
)

// Parse args:
func init() {
	flag.StringVar(&hosts, "hosts", "127.0.0.1", "AliTSDB hosts, comma-separated. Will be used in a round-robin fashion.")
	flag.IntVar(&port, "port", 8242, "AliTSDB listening port")
	flag.StringVar(&useCase, "use-case", common.UseCaseChoices[3], fmt.Sprintf("Use case to model. (choices: %s)", strings.Join(common.UseCaseChoices, ", ")))
	flag.IntVar(&batchSize, "batch-size", 1000, "Batch size (input lines).")
	flag.IntVar(&workers, "workers", 1, "Number of parallel requests to make.")
	//flag.DurationVar(&backoff, "backoff", time.Second, "Time to sleep between requests when server indicates backpressure is needed.")
	flag.BoolVar(&jsonFormat, "json-format", true, "If the input format is JSON or BINARY.")
	flag.BoolVar(&doLoad, "do-load", true, "Whether to write data. Set this flag to false to check input read speed.")
	flag.BoolVar(&memprofile, "memprofile", false, "Whether to write a memprofile (file automatically determined).")
	flag.StringVar(&cpuProfile, "cpu-profile", "", "Write CPU profile to `file`")
	flag.BoolVar(&viaHTTP, "viahttp", true, "Whether to write data via the HTTP protocol and whether to load data according to the JSON format")
	flag.StringVar(&reportDatabase, "report-database", "database_benchmarks", "Database name where to store result metrics")
	flag.StringVar(&reportHost, "report-host", "", "Host to send result metrics")
	flag.StringVar(&reportUser, "report-user", "", "User for host to send result metrics")
	flag.StringVar(&reportPassword, "report-password", "", "User password for Host to send result metrics")
	flag.StringVar(&reportTagsCSV, "report-tags", "", "Comma separated k:v tags to send  alongside result metrics")
	flag.BoolVar(&debug, "debug", false, "whether to print some debug information")
	flag.Parse()

	daemonUrls = strings.Split(hosts, ",")
	if len(daemonUrls) == 0 {
		log.Fatal("missing 'urls' flag")
	}
	fmt.Printf("daemon URLs: %v\n", daemonUrls)

	if reportHost != "" {
		fmt.Printf("results report destination: %v\n", reportHost)
		fmt.Printf("results report database: %v\n", reportDatabase)

		var err error
		reportHostname, err = os.Hostname()
		if err != nil {
			log.Fatalf("os.Hostname() error: %s", err.Error())
		}
		fmt.Printf("hostname for results report: %v\n", reportHostname)

		if reportTagsCSV != "" {
			pairs := strings.Split(reportTagsCSV, ",")
			for _, pair := range pairs {
				fields := strings.SplitN(pair, ":", 2)
				tagpair := [2]string{fields[0], fields[1]}
				reportTags = append(reportTags, tagpair)
			}
		}
		fmt.Printf("results report tags: %v\n", reportTags)
	}

	switch useCase {
	case common.UseCaseChoices[0]:
		fallthrough
	case common.UseCaseChoices[1]:
		fallthrough
	case common.UseCaseChoices[2]:
		log.Fatalf("Fields number not known")
	case common.UseCaseChoices[3]:
		FieldsNum = len(vehicle.EntityFieldKeys)
	default:
		log.Fatalf("Use case '%s' not supported", useCase)
	}
}

func main() {
	if cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	if memprofile {
		p := profile.Start(profile.MemProfile)
		defer p.Stop()
	}
	if doLoad {
		// check that there are no pre-existing databases:
		existingDatabases, err := listDatabases(daemonUrls[0])
		if err != nil {
			log.Fatal(err)
		}

		if len(existingDatabases) > 0 {
			log.Fatalf("There are databases already in the data store. If you know what you are doing, run the command:\ncurl 'http://localhost:8086/query?q=drop%%20database%%20%s'\n", existingDatabases[0])
		}
	}

	bufPool = sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, 4*1024*1024))
		},
	}

	batchChan = make(chan *bytes.Buffer, workers*batchSize)
	batchPointsChan = make(chan []*alitsdb_serialization.MultifieldPoint, workers*batchSize)
	monitorChan = make(chan bool)

	inputDone = make(chan struct{})

	backingOffChan = make(chan bool, 100)
	backingOffDone = make(chan struct{})

	for i := 0; i < workers; i++ {
		daemonURL := daemonUrls[i%len(daemonUrls)]
		workersGroup.Add(1)
		var writer LineProtocolWriter

		cfg := WriterConfig{
			Host: daemonURL,
			Port: port,
		}
		if viaHTTP {
			writer = NewHTTPWriter(cfg)
		} else {
			writer = NewRPCWriter(cfg)
		}
		go writer.ProcessBatches(doLoad, &bufPool, &workersGroup, backoff, backingOffChan)
	}

	go processBackoffMessages()

	if debug {
		// monitoring the channel
		go channelMonitor()
	}

	start := time.Now()
	var itemsRead, valuesRead int64

	//TODO: currently read json format only
	/*
			if viaHTTP {
				itemsRead, valuesRead = scanJSONfileForHTTP(batchSize)
			} else {
				itemsRead, valuesRead = scanBinaryfile(batchSize)
		    }
	*/
	if jsonFormat {
		if viaHTTP {
			itemsRead, valuesRead = scanJSONfileForHTTP(batchSize)
		} else {
			//itemsRead, valuesRead = scanJSONfileForGRPC(batchSize)
			log.Fatalln("not support JSON format when using RPC.")
		}
	} else {
		if viaHTTP {
			log.Fatalln("not support Binary format when using HTTP.")
		} else {
			itemsRead, valuesRead = scanBinaryfile(batchSize)
		}
	}

	<-inputDone
	close(batchChan)
	close(batchPointsChan)

	workersGroup.Wait()

	if debug {
		// ask the channel monitor routine to stop
		monitorChan <- true
	}

	close(backingOffChan)
	<-backingOffDone

	if debug {
		log.Println("monitor goroutine killed")
	}

	end := time.Now()
	took := end.Sub(start)
	itemrate := float64(itemsRead) / float64(took.Seconds())
	valuerate := float64(valuesRead) / float64(took.Seconds())

	// the output start time and end time are all in seconds
	fmt.Printf("loaded %d items and %d values in %fsec (start %d, end %d) with %d workers (mean point rate %f items/sec, value rate %f/s)\n",
		itemsRead, valuesRead, took.Seconds(), start.Unix(), end.Unix(), workers, itemrate, valuerate)

	if reportHost != "" {
		reportParams := &report.LoadReportParams{
			ReportParams: report.ReportParams{
				DBType:             "AliTSDB",
				ReportDatabaseName: reportDatabase,
				ReportHost:         reportHost,
				ReportUser:         reportUser,
				ReportPassword:     reportPassword,
				ReportTags:         reportTags,
				Hostname:           reportHostname,
				DestinationUrl:     daemonUrls[0],
				Workers:            workers,
				ItemLimit:          -1,
			},
			IsGzip:    true,
			BatchSize: batchSize,
		}
		err := report.ReportLoadResult(reportParams, itemsRead, valuerate, -1, took)

		if err != nil {
			log.Fatal(err)
		}
	}
}

// scanJSONfileForHTTP reads one line at a time from stdin.
// When the requested number of lines per batch is met, send a batch over batchChan for the workers to write.
func scanJSONfileForHTTP(linesPerBatch int) (int64, int64) {
	buf := bufPool.Get().(*bytes.Buffer)
	zw := gzip.NewWriter(buf)

	var n int
	var itemsRead int64

	zw.Write(openbracket)
	zw.Write(newline)

	scanner := bufio.NewScanner(bufio.NewReaderSize(os.Stdin, 4*1024*1024))
	for scanner.Scan() {
		itemsRead++
		if n > 0 {
			zw.Write(commaspace)
			zw.Write(newline)
		}

		zw.Write(scanner.Bytes())

		n++
		if n >= linesPerBatch {
			zw.Write(newline)
			zw.Write(closebracket)
			zw.Close()

			batchChan <- buf

			buf = bufPool.Get().(*bytes.Buffer)
			zw = gzip.NewWriter(buf)
			zw.Write(openbracket)
			zw.Write(newline)
			n = 0
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("Error reading input: %s", err.Error())
	}

	// Finished reading input, make sure last batch goes out.
	if n > 0 {
		zw.Write(newline)
		zw.Write(closebracket)
		zw.Close()
		batchChan <- buf
	}

	// Closing inputDone signals to the application that we've read everything and can now shut down.
	close(inputDone)

	return itemsRead, (itemsRead * int64(FieldsNum))
}

// scan reads one line at a time from stdin.
// When the requested number of lines per batch is met, send a batch over batchChan for the workers to write.
func scanBinaryfile(itemsPerBatch int) (int64, int64) {
	var itemsRead, bytesRead int64
	var err error
	var size uint64
	var n int
	//TODO:
	buff := make([]*alitsdb_serialization.MultifieldPoint, 0, itemsPerBatch)
	byteBuff := make([]byte, 100*1024)
	reader := bufio.NewReaderSize(os.Stdin, 4*1024*1024)
	for {
		err = binary.Read(reader, binary.LittleEndian, &size)
		if err == io.EOF {
			break
		}

		if err != nil {
			log.Fatalf("cannot read size of %d item: %v\n", itemsRead, err)
		}

		if uint64(cap(byteBuff)) < size {
			byteBuff = make([]byte, size)
		}

		bytesPerItem := uint64(0)
		for i := 10; i > 0; i-- {
			r, err := reader.Read(byteBuff[bytesPerItem:size])
			if err != nil && err != io.EOF {
				log.Fatalf("cannot read %d item: %v\n", itemsRead, err)
			}
			bytesPerItem += uint64(r)
			if bytesPerItem == size {
				break
			}
		}

		if bytesPerItem != size {
			log.Fatalf("cannot read %d item: read %d, expected %d\n", itemsRead, bytesPerItem, size)
		}
		basePoint := new(alitsdb_serialization.MultifieldPoint)

		err = basePoint.Unmarshal(byteBuff[:size])
		if err != nil {
			log.Fatalf("cannot unmarshall %d item: %v\n", itemsRead, err)
		}

		bytesRead += int64(size) + 8

		buff = append(buff, basePoint)
		itemsRead++
		n++

		if n > 0 && (n >= itemsPerBatch) {
			batchPointsChan <- buff
			n = 0
			buff = nil
			buff = make([]*alitsdb_serialization.MultifieldPoint, 0, itemsPerBatch)
		}
	}

	if err != nil && err != io.EOF {
		log.Fatalf("Error reading input after %d items: %s", itemsRead, err.Error())
	}

	// Finished reading input, make sure last batch goes out.
	if n > 0 {
		batchPointsChan <- buff
		buff = nil
	}

	// Closing inputDone signals to the application that we've read everything and can now shut down.
	close(inputDone)

	return itemsRead, (itemsRead * int64(FieldsNum))
}

func processBackoffMessages() {
	var totalBackoffSecs float64
	var start time.Time
	last := false
	for this := range backingOffChan {
		if this && !last {
			start = time.Now()
			last = true
		} else if !this && last {
			took := time.Now().Sub(start)
			fmt.Printf("backoff took %.02fsec\n", took.Seconds())
			totalBackoffSecs += took.Seconds()
			last = false
			start = time.Now()
		}
	}
	fmt.Printf("backoffs took a total of %fsec of runtime\n", totalBackoffSecs)
	backingOffDone <- struct{}{}
}

func channelMonitor() {
	for {
		select {
		case <-monitorChan:
			log.Printf("killing monitor routine...\n")
			return
		default:
			if viaHTTP {
				log.Printf("batchChan stats. Capacity: %d, Accumulation: %d  count of goroutines: %d\n",
					cap(batchChan), len(batchChan), runtime.NumGoroutine())
			} else {
				log.Printf("batchChan stats. Capacity: %d, Accumulation: %d  count of goroutines: %d\n",
					cap(batchPointsChan), len(batchPointsChan), runtime.NumGoroutine())
			}

			time.Sleep(10 * time.Second)
		}
	}
}

// TODO(rw): listDatabases lists the existing data in OpenTSDB.
func listDatabases(daemonUrl string) ([]string, error) {
	return nil, nil
}
