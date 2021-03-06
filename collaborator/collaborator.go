package collaborator

import (
	"fmt"
	"github.com/GoCollaborate/src/artifacts/card"
	"github.com/GoCollaborate/src/artifacts/iexecutor"
	"github.com/GoCollaborate/src/artifacts/iremote"
	"github.com/GoCollaborate/src/artifacts/iworkable"
	"github.com/GoCollaborate/src/artifacts/message"
	"github.com/GoCollaborate/src/artifacts/service"
	"github.com/GoCollaborate/src/artifacts/stats"
	"github.com/GoCollaborate/src/artifacts/task"
	"github.com/GoCollaborate/src/cmd"
	"github.com/GoCollaborate/src/constants"
	"github.com/GoCollaborate/src/logger"
	"github.com/GoCollaborate/src/store"
	"github.com/GoCollaborate/src/utils"
	"github.com/GoCollaborate/src/web"
	"github.com/GoCollaborate/src/wrappers/cardHelper"
	"github.com/GoCollaborate/src/wrappers/messageHelper"
	"github.com/GoCollaborate/src/wrappers/serviceHelper"
	"github.com/gorilla/mux"
	"net"
	"net/http"
	"net/rpc"
	"strconv"
	"time"
)

const (
	MAXIDLECONNECTIONS int           = 20
	REQUESTTIMEOUT     time.Duration = 5
	UPDATEINTERVAL     time.Duration = 1
)

// Collaborator is a helper struct to operate on any inner trxs.
type Collaborator struct {
	CardCase Case
	Workable iworkable.Workable
}

func NewCollaborator() *Collaborator {
	return &Collaborator{*newCase(), iworkable.Dummy()}
}

func newCase() *Case {
	return &Case{cmd.Vars().CaseID, &Exposed{make(map[string]card.Card), time.Now().Unix()}, &Reserved{*card.Default(), card.Card{}}}
}

// Join a master to the collaborator network.
func (clbt *Collaborator) Join(wk iworkable.Workable) {
	clbt.Workable = wk

	err := clbt.CardCase.readStream()
	if err != nil {
		panic(err)
	}

	// update if the read in timestamp is later than now
	if now := time.Now().Unix(); clbt.CardCase.Digest().TimeStamp() > now {
		clbt.CardCase.TimeStamp = now
		logger.LogWarning("The read in timestamp conflicts with local clock")
		logger.GetLoggerInstance().LogNormal("The read in timestamp conflicts with local clock")
	}

	logger.LogNormal("Case Identifier:")
	logger.GetLoggerInstance().LogNormal("Case Identifier:")
	logger.LogListPoint(clbt.CardCase.CaseID)
	logger.GetLoggerInstance().LogListPoint(clbt.CardCase.CaseID)

	ip := utils.GetLocalIP()

	cards := clbt.CardCase.Digest().Cards()

	logger.LogNormal("Cards:")
	logger.GetLoggerInstance().LogNormal("Cards:")
	cardHelper.RangePrint(cards)

	local := clbt.CardCase.Local

	// update if self does not exist
	if card, ok := clbt.CardCase.Cards[local.GetFullIP()]; !ok || !card.IsEqualTo(&local) {

		clbt.CardCase.Cards[local.GetFullIP()] = local
		clbt.CardCase.Stamp()
	}

	local.IP = ip

	logger.LogNormal("Local:")
	logger.GetLoggerInstance().LogNormal("Local:")
	cardHelper.RangePrint(map[string]card.Card{local.GetFullIP(): local})

	// start active message handler
	clbt.CardCase.Action()

	clbt.launchServer()

	go func() {
		for {
			clbt.Catchup()
			<-time.After(constants.DefaultSyncInterval)
			// clean collaborators that are no longer alive
			clbt.Clean()
			// dump data to local store
			clbt.CardCase.writeStream()
		}
	}()

	go func() {
		// service registry
		services, err := clbt.RegisterService()
		connected := err == nil
		// service mornitoring
		for connected {
			<-time.After(constants.DefaultHeartbeatInterval)
			clbt.HeartBeat(services)
		}
	}()
}

// Catchup with peer servers.
func (clbt *Collaborator) Catchup() {
	var (
		c = &clbt.CardCase

		dgst  = c.Digest()
		cards = dgst.Cards()

		max  = cmd.Vars().GossipNum
		done = 0
		seed = 0
	)

	for key, e := range cards {
		// stop gossipping if already walk through max nodes
		if done > max && seed > 0 {
			break
		}

		if e.Alive && (!e.IsEqualTo(&c.Local)) {
			client, err := launchClient(e.IP, e.Port)

			if err != nil {
				logger.LogWarning("Connection failed while bridging")
				logger.GetLoggerInstance().LogWarning("Connection failed while bridging")
				override := c.Cards[key]
				override.Alive = false
				c.Cards[key] = override
				c.writeStream()
				continue
			}

			from := c.Local.GetFullExposureCard()
			to := e
			var in *message.CardMessage = message.NewCardMessageWithOptions(c.Cluster(), from, to, dgst.Cards(), dgst.TimeStamp(), iremote.MsgTypeSync)
			var out *message.CardMessage = message.NewCardMessage()
			var out2 *message.CardMessage = message.NewCardMessage()
			var out3 *message.CardMessage = message.NewCardMessage()
			// first exchange
			err = client.Exchange(in, out)
			// local update exchange
			err = messageHelper.Exchange(out, out2)
			// second exchange
			err = client.Exchange(out2, out3)
			if err != nil {
				logger.LogWarning("Calling method failed while bridging")
				logger.GetLoggerInstance().LogWarning("Calling method failed while bridging")
				continue
			}
		}

		// if the collaborator is not a seed, server should attempt to contact a seed
		if e.IsSeed() {
			seed++
		}
		done++
	}
}

// Clean up the case, release terminated servers.
func (clbt *Collaborator) Clean() {

	cards := clbt.CardCase.Digest().Cards()

	for k, c := range cards {
		if !c.Alive {
			clbt.CardCase.Terminate(k)
		}
	}

	cards = clbt.CardCase.Digest().Cards()

	cardHelper.RangePrint(cards)
}

func (clbt *Collaborator) RegisterService() (map[string]*service.Service, error) {
	var (
		cdntIP   = clbt.CardCase.Coordinator.GetFullIP()
		clbtIP   = clbt.CardCase.Local.GetFullIP()
		services = map[string]*service.Service{}
	)

	resp, err := http.Get("http://" + cdntIP + "/cluster/" + clbt.CardCase.CaseID + "/services")
	if err != nil {
		return services, err
	}

	// if cluster already get created, return to call general service registry api on coordinator
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return services, err
	}

	var (
		router = store.GetRouter()
	)

	router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		t, err := route.GetPathTemplate()
		if err != nil {
			return err
		}
		svr := service.NewService()
		svr.Description = route.GetName()
		svr.RegList = append(svr.RegList, card.Card{
			IP:    clbt.CardCase.Local.IP,
			Port:  clbt.CardCase.Local.Port,
			API:   t,
			Alive: true,
		})
		services[clbtIP+t] = svr
		return nil
	})

	reader, err := serviceHelper.MarshalServiceResourceToByteStreamReader(services)
	if err != nil {
		return services, err
	}

	// walk through services and get them created on the coordinator
	resp2, err := http.Post("http://"+cdntIP+"/services", "application/json", reader)
	if err != nil {
		return services, err
	}

	_, err = http.Post("http://"+cdntIP+"/cluster/"+clbt.CardCase.CaseID+"/services", "application/json", resp2.Body)
	if err != nil {
		return services, err
	}

	return services, nil
}

func (clbt *Collaborator) HeartBeat(services map[string]*service.Service) {
	cdntIP := clbt.CardCase.Coordinator.GetFullIP()

	reader, err := serviceHelper.MarshalHeartbeatToByteStreamReader(&clbt.CardCase.Local, services)
	if err != nil {
		panic(err)
	}

	http.Post("http://"+cdntIP+"/services/heartbeat", "application/json", reader)
}

// Start handling server routes.
func (clbt *Collaborator) Handle(router *mux.Router) *mux.Router {

	// register dashboard
	router.HandleFunc("/", web.Index).Methods("GET").Name("Index")
	router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir(constants.LibUnixDir+"static/")))).Name("Static")
	router.HandleFunc("/dashboard/profile", web.Profile).Methods("GET").Name("Profile")
	router.HandleFunc("/dashboard/routes", web.Routes).Methods("GET").Name("Routes")
	router.HandleFunc("/dashboard/logs", web.Logs).Methods("GET").Name("Logs")
	router.HandleFunc("/dashboard/stats", web.Stats).Methods("GET").Name("Stats")

	// register tasks existing in publisher
	// reflect api endpoint based on exposed task (function) name
	// once api get called, use distribute
	fs := store.GetInstance()
	for _, jobFunc := range fs.SharedJobs {
		logger.LogNormal("Job Linked:")
		logger.LogListPoint(jobFunc.Signature)
		logger.GetLoggerInstance().LogNormal("Job Linked:")
		logger.GetLoggerInstance().LogListPoint(jobFunc.Signature)

		// shared registry
		clbt.HandleShared(router, jobFunc)
	}

	for _, jobFunc := range fs.LocalJobs {
		logger.LogNormal("Job Linked:")
		logger.LogListPoint(jobFunc.Signature)
		logger.GetLoggerInstance().LogNormal("Job Linked:")
		logger.GetLoggerInstance().LogListPoint(jobFunc.Signature)
		// local registry
		clbt.HandleLocal(router, jobFunc)
	}
	return router
}

// Distribute tasks to peer servers, this is a synchronized function.
func (clbt *Collaborator) DistributeSync(sources map[int]*task.Task) (map[int]*task.Task, error) {

	var (
		cc                         = clbt.CardCase
		dgst                       = cc.Digest()
		l1                         = len(sources)
		l2                         = len(dgst.Cards())
		result  map[int]*task.Task = make(map[int]*task.Task)
		counter int                = 0
	)

	// task channel
	chs := make(map[int]chan *task.Task)

	if l2 < 1 {
		return result, constants.ErrNoPeers
	}
	// waiting for rpc responses
	printProgress(counter, l1)

	for k, v := range sources {
		var l = k % l2
		e := cc.ReturnByPos(l)

		// publish to local if local Card/Card is down
		if e.IsEqualTo(&cc.Local) || !e.Alive {
			// copy a pointer for concurrent map access
			p := *v
			chs[k] = clbt.DelayExecute(&p)
			continue
		}

		// publish to remote
		client, err := launchClient(e.IP, e.Port)
		if err != nil {
			// re-publish to local if failed
			logger.LogWarning("Connection failed while connecting")
			logger.LogWarning("Republish task to local")
			logger.GetLoggerInstance().LogWarning("Connection failed while connecting")
			logger.GetLoggerInstance().LogWarning("Republish task to local")
			// copy a pointer for concurrent map access
			p := *v
			chs[k] = clbt.DelayExecute(&p)
			continue
		}

		// copy a pointer for concurrent map access
		s := make(map[int]*task.Task)
		s[k] = v
		chs[k] = client.DistributeSync(&s, &s)
	}

	// Wait for responses
	for {
		for i, ch := range chs {
			select {
			case t := <-ch:
				counter++
				result[i] = t
				printProgress(counter, l1)
			}
		}
		if counter >= l1 {
			break
		}
	}

	logger.LogProgress("All task responses collected")
	logger.GetLoggerInstance().LogProgress("All task responses collected")
	return result, nil
}

func registerRemote(server *rpc.Server, remote RemoteMethods) {
	server.RegisterName("RemoteMethods", remote)
}

func (c *Collaborator) launchServer() {
	go func() {
		methods := NewLocalMethods(c.Workable)
		server := rpc.NewServer()
		registerRemote(server, methods)
		// debug setup for regcenter to check services alive
		server.HandleHTTP("/", "debug")
		l, e := net.Listen("tcp", ":"+strconv.Itoa(c.CardCase.Local.Port))
		if e != nil {
			logger.LogError("Listen Error:", e)
			logger.GetLoggerInstance().LogError("Listen Error:", e)
		}
		http.Serve(l, nil)
	}()
	return
}

func launchClient(endpoint string, port int) (*RPCClient, error) {
	clientContact := card.Card{endpoint, port, true, "", false}
	client, err := rpc.DialHTTP("tcp", clientContact.GetFullIP())
	if err != nil {
		logger.LogError("Dialing:", err)
		logger.GetLoggerInstance().LogError("Dialing:", err)
		return &RPCClient{}, err
	}
	cWrapper := &RPCClient{Client: client}
	return cWrapper, nil
}

// Handle local Job routes.
func (clbt *Collaborator) HandleLocal(router *mux.Router, jobFunc *store.JobFunc) {

	var (
		f = func(w http.ResponseWriter, r *http.Request) {
			job := jobFunc.F(w, r)
			var (
				counter = 0
			)
			for s := job.Front(); s != nil; s = s.Next() {
				exes, err := job.Exes(counter)
				if err != nil {
					logger.GetLoggerInstance().LogError(err)
					break
				}
				err = clbt.LocalDistribute(&s.TaskSet, exes)
				if err != nil {
					logger.GetLoggerInstance().LogError(err)
					break
				}
				counter++
			}
		}
		fs = store.GetInstance()
	)

	lim, err := fs.GetLimiter(jobFunc.Signature)
	if err == nil {
		f = utils.AdaptLimiter(lim, f)
	}

	f = utils.AdaptStatsHits(f)

	f = utils.AdaptStatsRouteHits(jobFunc.Signature, f)

	router.HandleFunc(jobFunc.Signature, f).Methods(jobFunc.Methods...).Name("Local Tasks")
}

// Handle shared Job routes.
func (clbt *Collaborator) HandleShared(router *mux.Router, jobFunc *store.JobFunc) {

	var (
		f = func(w http.ResponseWriter, r *http.Request) {
			job := jobFunc.F(w, r)
			var (
				counter = 0
			)
			for s := job.Front(); s != nil; s = s.Next() {
				exes, err := job.Exes(counter)
				if err != nil {
					logger.GetLoggerInstance().LogError(err)
					break
				}

				err = clbt.SharedDistribute(&s.TaskSet, exes)
				if err != nil {
					logger.GetLoggerInstance().LogError(err)
					break
				}
				counter++
			}
		}
		fs = store.GetInstance()
	)

	lim, err := fs.GetLimiter(jobFunc.Signature)
	if err == nil {
		f = utils.AdaptLimiter(lim, f)
	}

	f = utils.AdaptStatsHits(f)

	f = utils.AdaptStatsRouteHits(jobFunc.Signature, f)

	router.HandleFunc(jobFunc.Signature, f).Methods(jobFunc.Methods...).Name("Shared Tasks")
}

// The function will process the tasks locally.
func (clbt *Collaborator) LocalDistribute(pmaps *map[int]*task.Task, stacks []string) error {

	// stats
	sm := stats.GetStatsInstance()
	sm.Record("tasks", len(*pmaps))

	var (
		err  error
		fs   = store.GetInstance()
		maps = *pmaps
	)

	for _, stack := range stacks {
		var (
			exe iexecutor.IExecutor
		)
		exe, err = fs.GetExecutor(stack)

		if err != nil {
			return err
		}

		switch exe.Type() {
		// mapper
		case constants.ExecutorTypeMapper:
			maps, err = exe.Execute(maps)
			if err != nil {
				return err
			}
		// reducer
		case constants.ExecutorTypeReducer:
			maps, err = exe.Execute(maps)
			if err != nil {
				return err
			}
		default:
			maps, err = exe.Execute(maps)
			if err != nil {
				return err
			}
		}
	}
	*pmaps = maps
	clbt.Workable.Enqueue(*pmaps)

	return nil
}

// The function will process the tasks globally within the cluster network.
func (clbt *Collaborator) SharedDistribute(pmaps *map[int]*task.Task, stacks []string) error {

	// stats
	sm := stats.GetStatsInstance()
	sm.Record("tasks", len(*pmaps))

	var (
		err  error
		fs   = store.GetInstance()
		maps = *pmaps
	)
	for _, stack := range stacks {
		var (
			exe iexecutor.IExecutor
		)
		exe, err = fs.GetExecutor(stack)

		if err != nil {
			return err
		}

		switch exe.Type() {
		// mapper
		case constants.ExecutorTypeMapper:
			maps, err = exe.Execute(maps)
			if err != nil {
				return err
			}
			maps, err = clbt.DistributeSync(maps)
			// reducer
		case constants.ExecutorTypeReducer:
			maps, err = exe.Execute(maps)
			if err != nil {
				return err
			}
		default:
			maps, err = exe.Execute(maps)
			if err != nil {
				return err
			}
		}
	}
	*pmaps = maps

	return nil
}

// Execute the tasks after satisfying some certain conditions.
func (clbt *Collaborator) DelayExecute(t *task.Task) chan *task.Task {
	ch := make(chan *task.Task)

	go func() {
		defer close(ch)
		err := clbt.Workable.Done(t)
		if err != nil {
			logger.LogError("Execution Error:" + err.Error())
			ch <- &task.Task{}
		}
		ch <- t
	}()
	return ch
}

func Delay(sec time.Duration) {
	tm := time.NewTimer(sec * time.Second)
	<-tm.C
}

func printProgress(num interface{}, tol interface{}) {
	logger.LogProgress("Waiting for task responses[" + fmt.Sprintf("%v", num) + "/" + fmt.Sprintf("%v", tol) + "]")
	logger.GetLoggerInstance().LogProgress("Waiting for task responses[" + fmt.Sprintf("%v", num) + "/" + fmt.Sprintf("%v", tol) + "]")
}
