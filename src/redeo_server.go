package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/ScottMansfield/nanolog"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/wangaoone/redeo"
	"github.com/wangaoone/redeo/resp"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const MaxLambdaStores = 14
const LambdaStoreName = "LambdaStore"

var (
	replica       = flag.Bool("replica", true, "Enable lambda replica deployment")
	isPrint       = flag.Bool("isPrint", false, "Enable log printing")
	prefix        = flag.String("prefix", "log", "log file prefix")
	dataCollected sync.WaitGroup
)

var (
	lambdaLis net.Listener
	//cMap      = make(map[int]chan interface{}) // client channel mapping table
	//cMap      = hashmap.New(1024 * 1024)
	cMap      = make([]chan interface{}, 1024*1024)
	filePath  = "/tmp/pidLog.txt"
	timeStamp = time.Now()
	reqMap    = make(map[string]*dataEntry)
	logMu     sync.Mutex
)

type dataEntry struct {
	cmd           string
	reqId         string
	chunkId       int64
	start         int64
	duration      int64
	firstByte     int64
	lambda2Server int64
	server2Client int64
	readBulk      int64
	appendBulk    int64
	flush         int64
}

func nanoLog(handle nanolog.Handle, args ...interface{}) error {
	timeStamp = time.Now()
	key := fmt.Sprintf("%s-%s-%d", args[0], args[1], args[2])
	if handle == resp.LogStart {
		logMu.Lock()
		reqMap[key] = &dataEntry{
			cmd:     args[0].(string),
			reqId:   args[1].(string),
			chunkId: args[2].(int64),
			start:   args[3].(int64),
		}
		logMu.Unlock()
		return nil
	} else if handle == resp.LogProxy {
		logMu.Lock()
		entry := reqMap[key]
		logMu.Unlock()

		entry.firstByte = args[3].(int64) - entry.start
		args[3] = entry.firstByte
		entry.lambda2Server = args[4].(int64)
		entry.readBulk = args[5].(int64)
		return nil
	} else if handle == resp.LogServer2Client {
		logMu.Lock()
		entry := reqMap[key]
		//delete(reqMap, key)
		logMu.Unlock()

		entry.server2Client = args[3].(int64)
		entry.appendBulk = args[4].(int64)
		entry.flush = args[5].(int64)
		entry.duration = args[6].(int64) - entry.start

		return nanolog.Log(resp.LogData, entry.cmd, entry.reqId, entry.chunkId,
			entry.start, entry.duration,
			entry.firstByte, entry.lambda2Server, entry.server2Client,
			entry.readBulk, entry.appendBulk, entry.flush)
	}

	return nanolog.Log(handle, args...)
}

func logCreate() {
	// get local time
	//location, _ := time.LoadLocation("EST")
	// Set up nanoLog writer
	//nanoLogout, err := os.Create("/tmp/proxy/" + *prefix + "_proxy.clog")
	nanoLogout, err := os.Create(*prefix + "_proxy.clog")
	if err != nil {
		panic(err)
	}
	err = nanolog.SetWriter(nanoLogout)
	if err != nil {
		panic(err)
	}
}

func main() {
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)
	//signal.Notify(sig, syscall.SIGINT)
	flag.Parse()
	// CPU profiling by default
	//defer profile.Start().Stop()
	// init log
	logCreate()

	fmt.Println("======================================")
	fmt.Println("replica:", *replica, "||", "isPrint:", *isPrint)
	fmt.Println("======================================")
	clientLis, err := net.Listen("tcp", ":6378")
	if err != nil {
		fmt.Println("client facing listen", err)
	}
	lambdaLis, err = net.Listen("tcp", ":6379")
	if err != nil {
		fmt.Println("lambda facing listen", err)
	}
	fmt.Println("start listening client face port :6378，lambda face port :6379")
	// initial proxy and lambda server
	srv := redeo.NewServer(nil)
	lambdaSrv := redeo.NewServer(nil)

	// initial lambda store group
	group := initial(lambdaSrv)

	err = ioutil.WriteFile(filePath, []byte(fmt.Sprintf("%d", os.Getpid())), 0660)
	if err != nil {
		fmt.Println(err)
	}
	fmt.Println("lambda store ready!")

	// Log goroutine
	//defer t.Stop()
	go func() {
		t := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-sig:
				t.Stop()
				if err := nanolog.Flush(); err != nil {
					fmt.Println("Failed to save data:", err)
				}

				// Collect data
				for _, node := range group.Arr {
					node.W.WriteCmdString("data")
					err := node.W.Flush()
					if err != nil {
						fmt.Println("Failed to submit data request:", err)
						continue
					}
					dataCollected.Add(1)
				}
				//fmt.Println("wait for data")
				dataCollected.Wait()
				if err := nanolog.Flush(); err != nil {
					fmt.Println("Failed to save data from lambdas:", err)
				}

				err := os.Remove(filePath)
				if err != nil {
					fmt.Println("Failed to remove pid:", err)
				}

				lambdaLis.Close()
				clientLis.Close()
				close(done)

				// Collect data

				return
			case <-t.C:
				if time.Since(timeStamp) >= 10*time.Second {
					if err := nanolog.Flush(); err != nil {
						fmt.Println("Failed to save data:", err)
					}
				}
			}
		}
	}()

	// Start serving (blocking)
	err = srv.MyServe(clientLis, cMap, group, nanoLog, done)
	if err != nil {
		fmt.Println(err)
	}
}

// initial lambda group
func initial(lambdaSrv *redeo.Server) redeo.Group {
	group := redeo.Group{Arr: make([]*redeo.LambdaInstance, MaxLambdaStores), MemCounter: 0}
	if *replica == true {
		for i := range group.Arr {
			node := newLambdaInstance(LambdaStoreName)
			myPrint("No.", i, "replication lambda store has registered")
			// register lambda instance to group
			group.Arr[i] = node
			node.Alive = true
			go lambdaTrigger(node)
			// start a new server to receive conn from lambda store
			node.Cn = lambdaSrv.Accept(lambdaLis)
			myPrint("start a new conn, lambda store has connected", node.Cn.RemoteAddr())
			// wrap writer and reader
			node.W = resp.NewRequestWriter(node.Cn)
			node.R = resp.NewResponseReader(node.Cn)
			// lambda handler
			go lambdaHandler(node)
			// lambda facing peeking response type
			go LambdaPeek(node)
			myPrint(node.Alive)
		}
	} else {
		for i := range group.Arr {
			node := newLambdaInstance("Node" + strconv.Itoa(i))
			myPrint(node.Name, "lambda store has registered")
			// register lambda instance to group
			group.Arr[i] = node
			node.Alive = true
			go lambdaTrigger(node)
			// start a new server to receive conn from lambda store
			node.Cn = lambdaSrv.Accept(lambdaLis)
			myPrint("start a new conn, lambda store has connected", node.Cn.RemoteAddr())
			// wrap writer and reader
			node.W = resp.NewRequestWriter(node.Cn)
			node.R = resp.NewResponseReader(node.Cn)
			// lambda handler
			go lambdaHandler(node)
			// lambda facing peeking response type
			go LambdaPeek(node)
			myPrint(node.Alive)
		}
	}
	return group
}

// create new lambda instance
func newLambdaInstance(name string) *redeo.LambdaInstance {
	return &redeo.LambdaInstance{
		Name:  name,
		Alive: false,
		C:     make(chan *redeo.ServerReq, 1024*1024),
	}
}

// blocking on lambda peek Type
// lambda handle incoming lambda store response
//
// field 0 : conn id
// field 1 : req id
// field 2 : chunk id
// field 3 : obj val

func LambdaPeek(l *redeo.LambdaInstance) {
	for {
		var obj redeo.Response
		// field 0 for cmd
		field0, err := l.R.PeekType()
		if err != nil {
			fmt.Println("field1 err", err)
			continue
		}
		t2 := time.Now()
		switch field0 {
		case resp.TypeBulk:
			cmd, _ := l.R.ReadBulkString()
			switch cmd {
			case "get":
			case "set":
				setHandler(l, t2)
				err = errors.New("continue")
			case "data":
				collectDataFromLambda(l)
				err = errors.New("continue")
			}
		case resp.TypeError:
			err, _ := l.R.ReadError()
			fmt.Println("peek type err1 is", err)
		default:
			panic("unexpected response type")
		}
		if err != nil {
			continue
		}
		// Get Handler
		// field 1 connId
		connId, _ := l.R.ReadBulkString()
		obj.Id.ConnId, _ = strconv.Atoi(connId)
		// field 1 for req id
		abandon := false
		reqId, _ := l.R.ReadBulkString()
		obj.Id.ReqId = reqId
		counter, ok := redeo.ReqMap.Get(reqId)
		if ok == false {
			fmt.Println("No reqId found")
		}
		reqCounter := atomic.AddInt32(&(counter.(*redeo.ClientReqCounter).Counter), 1)
		//myPrint("cmd is", counter.(*redeo.ClientReqCounter).Cmd, "atomic counter is", int(reqCounter), "dataShards int", counter.(*redeo.ClientReqCounter).DataShards)
		if int(reqCounter) > counter.(*redeo.ClientReqCounter).DataShards && counter.(*redeo.ClientReqCounter).Cmd == "get" {
			abandon = true
		}
		// field 3 for chunk id
		chunkId, _ := l.R.ReadBulkString()
		obj.Id.ChunkId, _ = strconv.ParseInt(chunkId, 10, 64)
		fmt.Println("Lambda peek chunkId is", obj.Id.ChunkId)
		// if abandon response, cmd must be GET
		if abandon {
			obj.Cmd = "get"
			cMap[obj.Id.ConnId] <- &redeo.Chunk{ChunkId: obj.Id.ChunkId, ReqId: obj.Id.ReqId, Cmd: "get"}
			if err := nanoLog(resp.LogProxy, obj.Cmd, obj.Id.ReqId, obj.Id.ChunkId, t2.UnixNano(), int64(time.Since(t2)), int64(0)); err != nil {
				fmt.Println("LogProxy err ", err)
			}
		}
		// field 3 for obj body
		t9 := time.Now()
		res, err := l.R.ReadBulk(nil)
		if err != nil {
			fmt.Println("response err is ", err)
		}
		if !abandon {
			cMap[obj.Id.ConnId] <- &redeo.Chunk{ChunkId: obj.Id.ChunkId, ReqId: obj.Id.ReqId, Body: res, Cmd: "get"}
		}
		time9 := time.Since(t9)
		// Skip log on abandon
		if abandon {
			continue
		}
		time0 := time.Since(t2)
		if err := nanoLog(resp.LogProxy, "get", obj.Id.ReqId, obj.Id.ChunkId, t2.UnixNano(), int64(time0), int64(time9)); err != nil {
			fmt.Println("LogProxy err ", err)
		}
	}
}

func setHandler(l *redeo.LambdaInstance, t time.Time) {
	var obj redeo.Response
	connId, _ := l.R.ReadBulkString()
	obj.Id.ConnId, _ = strconv.Atoi(connId)
	obj.Id.ReqId, _ = l.R.ReadBulkString()
	chunkId, _ := l.R.ReadBulkString()
	obj.Id.ChunkId, _ = strconv.ParseInt(chunkId, 10, 64)
	fmt.Println("lambda peek chunk id is ", obj.Id.ChunkId)
	cMap[obj.Id.ConnId] <- &redeo.Chunk{ChunkId: obj.Id.ChunkId, ReqId: obj.Id.ReqId, Body: []byte{1}, Cmd: "set"}
	if err := nanoLog(resp.LogProxy, "set", obj.Id.ReqId, obj.Id.ChunkId, t.UnixNano(), int64(time.Since(t)), int64(0)); err != nil {
		fmt.Println("LogProxy err ", err)
	}
}

// lambda Handler
// lambda handle incoming client request
func lambdaHandler(l *redeo.LambdaInstance) {
	myPrint("conn is", l.Cn)
	for {
		a := <-l.C /*blocking on lambda facing channel*/
		// check lambda status first
		l.AliveLock.Lock()
		if l.Alive == false {
			myPrint("Lambda store is not alive, need to activate")
			l.Alive = true
			// trigger lambda
			go lambdaTrigger(l)
		}
		l.AliveLock.Unlock()
		//*
		// req from client
		//*
		// get channel and chunk id
		connId := strconv.Itoa(a.Id.ConnId)
		chunkId := strconv.FormatInt(a.Id.ChunkId, 10)
		// get cmd argument
		cmd := strings.ToLower(a.Cmd)
		switch cmd {
		case "set": /*set or two argument cmd*/
			l.W.MyWriteCmd(a.Cmd, connId, a.Id.ReqId, chunkId, a.Key, a.Body)
			err := l.W.Flush()
			if err != nil {
				fmt.Println("flush pipeline err is ", err)
			}
		case "get": /*get or one argument cmd*/
			l.W.MyWriteCmd(a.Cmd, connId, a.Id.ReqId, "", a.Key)
			err := l.W.Flush()
			if err != nil {
				fmt.Println("flush pipeline err is ", err)
			}
		}
	}
}

func lambdaTrigger(l *redeo.LambdaInstance) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	client := lambda.New(sess, &aws.Config{Region: aws.String("us-east-1")})

	_, err := client.Invoke(&lambda.InvokeInput{FunctionName: aws.String(l.Name)})
	if err != nil {
		fmt.Println("Error calling LambdaFunction", err)
	}

	myPrint("Lambda Deactivate")
	l.AliveLock.Lock()
	l.Alive = false
	l.AliveLock.Unlock()
}

func myPrint(a ...interface{}) {
	if *isPrint {
		fmt.Println(a)
	}
}

func collectDataFromLambda(l *redeo.LambdaInstance) {
	len, err := l.R.ReadInt()
	if err != nil {
		fmt.Println("Failed to read length of data from lambda", err)
		return
	}
	for i := int64(0); i < len; i++ {
		op, _ := l.R.ReadBulkString()
		status, _ := l.R.ReadBulkString()
		reqId, _ := l.R.ReadBulkString()
		chunkId, _ := l.R.ReadBulkString()
		dAppend, _ := l.R.ReadInt()
		dFlush, _ := l.R.ReadInt()
		dTotal, _ := l.R.ReadInt()
		//fmt.Println("op, reqId, chunkId, status, dTotal, dAppend, dFlush", op, reqId, chunkId, status, dTotal, dAppend, dFlush)
		nanoLog(resp.LogLambda, "data", op, reqId, chunkId, status, dTotal, dAppend, dFlush)
	}
	dataCollected.Done()
}
