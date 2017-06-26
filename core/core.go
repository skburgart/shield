package core

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/starkandwayne/goutils/log"
	"github.com/starkandwayne/goutils/timestamp"
	"gopkg.in/yaml.v2"

	"github.com/starkandwayne/shield/db"
	"github.com/starkandwayne/shield/timespec"
)

var Version = "(development)"

type Core struct {
	fastloop *time.Ticker
	slowloop *time.Ticker

	timeout int
	agent   *AgentClient

	/* cached for /v2/health */
	ip   string
	fqdn string

	/* foreman */
	numWorkers int
	workers    chan *db.Task

	/* monitor */
	agents map[string]chan *db.Agent

	/* janitor */
	purgeAgent string

	/* api */
	webroot string
	listen  string

	DB *db.DB
}

func NewCore(file string) (*Core, error) {
	config := struct {
		SlowLoop int `yaml:"slow_loop"`
		FastLoop int `yaml:"fast_loop"`

		DBType        string `yaml:"database_type"`
		DBPath        string `yaml:"database_dsn"`
		Addr          string `yaml:"listen_addr"`
		KeyFile       string `yaml:"private_key"`
		Workers       int    `yaml:"workers"`
		Purge         string `yaml:"purge_agent"`
		Timeout       int    `yaml:"max_timeout"`
		SkipSSLVerify bool   `yaml:"skip_ssl_verify"`
		WebRoot       string `yaml:"web_root"`
	}{
		FastLoop: 1,
		SlowLoop: 60 * 5,

		DBPath:  "shield.db",
		Addr:    "*:8888",
		KeyFile: "worker.key",
		Workers: 2,
		Purge:   "localhost:5444",
		Timeout: 12,
		WebRoot: "web",
	}

	/* optionally read configuration from a file */
	if file != "" {
		b, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, err
		}

		if err = yaml.Unmarshal(b, &config); err != nil {
			return nil, err
		}
	}

	/* validate configuration */
	if config.FastLoop <= 0 {
		return nil, fmt.Errorf("fast_loop value '%d' is invalid (must be greater than zero)")
	}
	if config.SlowLoop <= 0 {
		return nil, fmt.Errorf("slow_loop value '%d' is invalid (must be greater than zero)")
	}
	if config.Timeout <= 0 {
		return nil, fmt.Errorf("timeout value '%d' is invalid (must be greater than zero)")
	}
	if config.Workers <= 0 {
		return nil, fmt.Errorf("number of workers '%d' is invalid (must be greater than zero)")
	}
	// FIXME: check existence of WebRoot

	agent, err := NewAgentClient(config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read agent key file %s: %s", config.KeyFile, err)
	}

	ip, fqdn := networkIdentity()

	return &Core{
		fastloop: time.NewTicker(time.Second * time.Duration(config.FastLoop)),
		slowloop: time.NewTicker(time.Second * time.Duration(config.SlowLoop)),

		timeout: config.Timeout,
		agent:   agent,

		ip:   ip,
		fqdn: fqdn,

		/* foreman */
		numWorkers: config.Workers,
		workers:    make(chan *db.Task),

		/* monitor */
		agents: make(map[string]chan *db.Agent),

		/* janitor */
		purgeAgent: config.Purge,

		/* api */
		webroot: config.WebRoot,
		listen:  config.Addr,

		DB: &db.DB{
			Driver: config.DBType,
			DSN:    config.DBPath,
		},
	}, nil
}

func (core *Core) Run() error {
	if err := core.DB.Connect(); err != nil {
		return fmt.Errorf("failed to connect to database: %s", err)
	}
	if err := core.DB.CheckCurrentSchema(); err != nil {
		return fmt.Errorf("database failed schema version check: %s", err)
	}

	core.cleanup()
	core.api()
	core.runWorkers()

	for {
		select {
		case <-core.fastloop.C:
			core.scheduleTasks()
			core.runPending()

		case <-core.slowloop.C:
			core.expireArchives()
			core.purge()
			core.markTasks()
			core.checkAgents()
		}
	}
}

func (core *Core) api() {
	http.Handle("/v1/", core)
	http.Handle("/v2/", core)
	http.Handle("/", http.FileServer(http.Dir(core.webroot)))
	// FIXME: no OAuth2 support yet...

	log.Infof("starting up api listener on %s", core.listen)
	go func() {
		err := http.ListenAndServe(core.listen, nil)
		if err != nil {
			log.Errorf("shield core api failed to start up: %s", err)
			os.Exit(2)
		}
		log.Infof("shutting down shield core api")
	}()
}

func (core *Core) runWorkers() {
	log.Infof("shield core spinning %d worker threads", core.numWorkers)
	for id := 1; id <= core.numWorkers; id++ {
		log.Debugf("spawning worker %d", id)
		go core.worker(id)
	}
}

func (core *Core) cleanup() {
	tasks, err := core.DB.GetAllTasks(&db.TaskFilter{ForStatus: db.RunningStatus})
	if err != nil {
		log.Errorf("failed to cleanup leftover running tasks: %s", err)
		return
	}

	now := time.Now()
	for _, task := range tasks {
		log.Warnf("found task %s in 'running' state at startup; setting to 'failed'", task.UUID)
		if err := core.DB.FailTask(task.UUID, now); err != nil {
			log.Errorf("failed to sweep database of running tasks [%s]: %s", task.UUID, err)
			continue
		}

		if task.Op == db.BackupOperation && task.ArchiveUUID != nil {
			archive, err := core.DB.GetArchive(task.ArchiveUUID)
			if err != nil {
				log.Warnf("unable to retrieve archive %s (for task %s) from the database: %s",
					task.ArchiveUUID, task.UUID, err)
				continue
			}
			log.Warnf("found archive %s for task %s, purging", archive.UUID, task.UUID)
			task, err := core.DB.CreatePurgeTask("", archive, core.purgeAgent)
			if err != nil {
				log.Errorf("failed to purge archive %s (for task %s, which was running at boot): %s",
					archive.UUID, task.UUID, err)
			}
		}
	}
}

func (core *Core) scheduleTasks() {
	l, err := core.DB.GetAllJobs(&db.JobFilter{Overdue: true})
	if err != nil {
		log.Errorf("error retrieving all overdue jobs from database: %s", err)
		return
	}

	for _, job := range l {
		log.Infof("scheduling a run of job %s [%s]", job.Name, job.UUID)
		core.DB.CreateBackupTask("system", job)

		if spec, err := timespec.Parse(job.Schedule); err != nil {
			log.Errorf("error re-scheduling job %s [%s]: %s", job.Name, job.UUID, err)
		} else {
			if next, err := spec.Next(time.Now()); err != nil {
				log.Errorf("error re-scheduling job %s [%s]: %s", job.Name, job.UUID, err)
			} else {
				if err = core.DB.RescheduleJob(job, next); err != nil {
					log.Errorf("error re-scheduling job %s [%s]: %s", job.Name, job.UUID, err)
				}
			}
		}
	}
}

func (core *Core) runPending() {
	l, err := core.DB.GetAllTasks(&db.TaskFilter{ForStatus: "pending"})
	if err != nil {
		log.Errorf("error retrieving pending tasks from database: %s", err)
		return
	}

	for _, task := range l {
		/* set up the deadline for execution */
		task.TimeoutAt = timestamp.Now().Add(time.Duration(core.timeout))
		log.Infof("schedule task %s with deadline %v", task.UUID, task.TimeoutAt)

		/* mark the task as scheduled, so we don't pick it up again */
		core.DB.ScheduledTask(task.UUID)

		/* spin up a goroutine so that we can block in the write
		   to the workers channel, yet return immediately to here,
		   and 'queue up' the remaining pending tasks */
		go func() {
			core.workers <- task
			log.Debugf("dispatched task %s to a worker goroutine", task.UUID)
		}()
	}
}

func (core *Core) expireArchives() {
	log.Debugf("scanning for archives that outlived their retention policy")
	l, err := core.DB.GetExpiredArchives()
	if err != nil {
		log.Errorf("error retrieving archives that have outlived their retention policy: %s", err)
		return

	}
	for _, archive := range l {
		log.Infof("marking archive %s has expiration date %s, marking as expired", archive.UUID, archive.ExpiresAt)
		err := core.DB.ExpireArchive(archive.UUID)
		if err != nil {
			log.Errorf("error marking archive %s as expired: %s", archive.UUID, err)
			continue
		}
	}
}

func (core *Core) purge() {
	log.Debugf("scanning for archvies that need purged")
	l, err := core.DB.GetArchivesNeedingPurge()
	if err != nil {
		log.Errorf("error retrieving archives to purge: %s", err)
		return
	}

	for _, archive := range l {
		log.Infof("requesting purge of archive %s due to status '%s'", archive.UUID, archive.Status)
		_, err := core.DB.CreatePurgeTask("system", archive, core.purgeAgent)
		if err != nil {
			log.Errorf("error scheduling purge of archive %s: %s", archive.UUID, err)
			continue
		}
	}
}

func (core *Core) markTasks() {
	core.DB.MarkTasksIrrelevant()
}

func (core *Core) checkAgents() {
	log.Debugf("scanning for agents that need to be checked")

	agents, err := core.DB.GetAllAgents(nil)
	if err != nil {
		log.Errorf("error retrieving agent registration records from database: %s", err)
		return
	}
	for _, a := range agents {
		if c, ok := core.agents[a.Address]; ok {
			select {
			case c <- a:
				log.Infof("monitor: dispatched agent health check for '%s' to a monitor thread", a.Address)

			default:
				log.Infof("monitor: dropped agent health check for '%s'; there is already an operation in-flight",
					a.Address)
			}
			return
		}

		/* spin up a new goroutine to this and future
		   health checks of this SHIELD agent */
		core.agents[a.Address] = make(chan *db.Agent)
		go func(in chan *db.Agent) {
			for a := range in {
				func() {
					stdout := make(chan string, 1)
					stderr := make(chan string)
					go func() {
						for s := range stderr {
							log.Debugf("  [monitor] %s> %s", a.Address, strings.Trim(s, "\n"))
						}
					}()

					if err := core.agent.Run(a.Address, stdout, stderr, &AgentCommand{Op: "status"}); err != nil {
						log.Errorf("  [monitor] %s: !! failed to run status op: %s", a.Address, err)

						a.Status = "failing"
						a.LastError = fmt.Sprintf("failed to run status op: %s", err)

						log.Debugf("  [monitor] %s> updating (agent=%s) with status '%s'...", a.Address, a.UUID, a.Status)
						if err := core.DB.UpdateAgent(a); err != nil {
							log.Errorf("  [monitor] %s: !! failed to update database: %s", a.Address, err)
						}
						return
					}

					response := <-stdout

					var x struct {
						Name    string `json:"name"`
						Version string `json:"version"`
						Health  string `json:"health"`
					}
					if err = json.Unmarshal([]byte(response), &x); err != nil {
						log.Errorf("  [monitor] %s: !! failed to parse status op response: %s", a.Address, err)

						a.Status = "failing"
						a.LastError = fmt.Sprintf("failed to parse status op response: %s", err)

						log.Debugf("  [monitor] %s> updating (agent=%s) with status '%s'...", a.Address, a.UUID, a.Status)
						if err := core.DB.UpdateAgent(a); err != nil {
							log.Errorf("  [monitor] %s: !! failed to update database: %s", a.Address, err)
						}
						return
					}

					if a.Name != x.Name {
						log.Errorf("  [monitor] %s: !! got response for agent '%s' (not '%s')", a.Address, x.Name, a.Name)

						a.Status = "degraded"
						a.LastError = fmt.Sprintf("got response for agent '%s' (not '%s')", x.Name, a.Name)

						log.Debugf("  [monitor] %s> updating (agent=%s) with status '%s'...", a.Address, a.UUID, a.Status)
						if err := core.DB.UpdateAgent(a); err != nil {
							log.Errorf("  [monitor] %s: !! failed to update database: %s", a.Address, err)
						}
						return
					}

					a.Status = x.Health
					a.Version = x.Version
					a.Metadata = response

					log.Debugf("  [monitor] %s> updating (agent=%s) with status '%s'...", a.Address, a.UUID, a.Status)
					if err := core.DB.UpdateAgent(a); err != nil {
						log.Errorf("  [monitor] %s: !! failed to update database: %s", a.Address, err)
					}
				}()
			}
		}(core.agents[a.Address])
		core.agents[a.Address] <- a
	}
}

func (core *Core) worker(id int) {
	/* read a task from the core */
	for task := range core.workers {
		log.Debugf("worker %d starting to execute task %s", id, task.UUID)

		if task.Agent == "" {
			err := core.DB.UpdateTaskLog(
				task.UUID,
				fmt.Sprintf("TASK FAILED!!  no remote agent specified for task %s", task.UUID),
			)
			if err != nil {
				log.Errorf("  %s: !! failed to update database: %s", task.UUID, err)
			}

			core.handleFailure(task)
			continue
		}

		stdout := make(chan string, 1)
		stderr := make(chan string)
		go func() {
			for s := range stderr {
				core.handleOutput(task, "%s", s)
			}
		}()

		/* connect to the remote SSH agent for this specific request
		   (a worker may connect to lots of different agents in its
		    lifetime; these connections endure long enough to submit
		    the agent command and gather the exit code + output) */
		err := core.agent.Run(task.Agent, stdout, stderr, &AgentCommand{
			Op:             task.Op,
			TargetPlugin:   task.TargetPlugin,
			TargetEndpoint: task.TargetEndpoint,
			StorePlugin:    task.StorePlugin,
			StoreEndpoint:  task.StoreEndpoint,
			RestoreKey:     task.RestoreKey,
		})
		if err != nil {
			core.handleOutput(task, "TASK FAILED!!  shield worker %d unable to run command against %s: %s\n", id, task.Agent, err)
			core.handleFailure(task)
			continue
		}

		failed := false
		response := <-stdout
		if task.Op == db.BackupOperation {
			var v struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal([]byte(response), &v); err != nil {
				failed = true
				core.handleOutput(task, "WORKER FAILED!!  shield worker %d failed to parse JSON response from remote agent %s (%s)\n", id, task.Agent, err)

			} else {
				if v.Key != "" {
					log.Infof("  %s: restore key is %s", task.UUID, v.Key)
					if id, err := core.DB.CreateTaskArchive(task.UUID, v.Key, time.Now()); err != nil {
						log.Errorf("  %s: !! failed to update database: %s", task.UUID, err)
					} else if failed {
						core.DB.InvalidateArchive(id)
					}

				} else {
					failed = true
					core.handleOutput(task, "TASK FAILED!! No restore key detected in worker %d. Cowardly refusing to create an archive record\n", id)
				}
			}
		}

		if task.Op == db.PurgeOperation && !failed {
			log.Infof("  %s: archive %s purged from storage", task.UUID, task.ArchiveUUID)
			if err := core.DB.PurgeArchive(task.ArchiveUUID); err != nil {
				log.Errorf("  %s: !! failed to update database: %s", task.UUID, err)
			}
		}

		if failed {
			core.handleFailure(task)
		} else {
			log.Infof("  %s: job completed successfully", task.UUID)
			if err := core.DB.CompleteTask(task.UUID, time.Now()); err != nil {
				log.Errorf("  %s: !! failed to update database: %s", task.UUID, err)
			}
		}
	}
}

func (core *Core) handleFailure(task *db.Task) {
	log.Warnf("  %s: task failed!", task.UUID)
	if err := core.DB.FailTask(task.UUID, time.Now()); err != nil {
		log.Errorf("  %s: !! failed to update database: %s", task.UUID, err)
	}
}

func (core *Core) handleOutput(task *db.Task, f string, args ...interface{}) {
	s := fmt.Sprintf(f, args...)
	log.Infof("  %s> %s", task.UUID, strings.Trim(s, "\n"))
	if err := core.DB.UpdateTaskLog(task.UUID, s); err != nil {
		log.Errorf("  %s: !! failed to update database: %s", task.UUID, err)
	}
}

func networkIdentity() (string, string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "(unknown)", ""
	}

	var v4ip, v6ip, host string

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var (
				found bool
				ip    net.IP
			)

			switch addr.(type) {
			case *net.IPNet:
				ip = addr.(*net.IPNet).IP
				found = !ip.IsLoopback()
			case *net.IPAddr:
				ip = addr.(*net.IPAddr).IP
				found = !ip.IsLoopback()
			}
			log.Debugf("net: found interface with address %s", ip.String())
			isv4 := ip.To4() != nil
			log.Debugf("net: (found=%v, isv4=%v, v4ip=%s, v6ip=%s)",
				found, isv4, v4ip, v6ip)
			if !found || (!isv4 && v6ip != "") || (isv4 && v4ip != "") {
				log.Debugf("net: SKIPPING")
				continue
			}

			if isv4 {
				v4ip = ip.String()
			} else {
				v6ip = ip.String()
			}

			names, err := net.LookupAddr(ip.String())
			if err != nil {
				continue
			}
			if len(names) != 0 {
				host = names[0]
			}
		}
	}

	if v4ip != "" {
		return v4ip, host
	}
	if v6ip != "" {
		return v6ip, host
	}
	return "(unknown)", ""
}
