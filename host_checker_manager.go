package main

import (
	b64 "encoding/base64"
	"github.com/lonelycode/go-uuid/uuid"
	"github.com/lonelycode/tykcommon"
	"gopkg.in/vmihailenco/msgpack.v2"
	"net/url"
	"time"
)

var GlobalHostChecker HostCheckerManager

type HostCheckerManager struct {
	Id                string
	store             *RedisClusterStorageManager
	checker           *HostUptimeChecker
	stopLoop          bool
	pollerStarted     bool
	unhealthyHostList map[string]bool
	currentHostList   map[string]HostData
	Clean             Purger
}

type UptimeReportData struct {
	URL          string
	RequestTime  int64
	ResponseCode int
	TCPError     bool
	ServerError  bool
	Day          int
	Month        time.Month
	Year         int
	Hour         int
	TimeStamp    time.Time
	ExpireAt     time.Time `bson:"expireAt" json:"expireAt"`
	APIID        string
	OrgID        string
}

func (u *UptimeReportData) SetExpiry(expiresInSeconds int64) {
	var expiry time.Duration

	expiry = time.Duration(expiresInSeconds) * time.Second

	if expiresInSeconds == 0 {
		// Expiry is set to 100 years
		expiry = (24 * time.Hour) * (365 * 100)
	}

	t := time.Now()
	t2 := t.Add(expiry)
	u.ExpireAt = t2
}

const (
	UnHealthyHostMetaDataTargetKey string = "target_url"
	UnHealthyHostMetaDataAPIKey    string = "api_id"
	UnHealthyHostMetaDataHostKey   string = "host_name"
	PollerCacheKey                 string = "PollerActiveInstanceID"
	PoolerHostSentinelKeyPrefix    string = "PollerCheckerInstance:"

	UptimeAnalytics_KEYNAME string = "tyk-uptime-analytics"
)

func (hc *HostCheckerManager) Init(store *RedisClusterStorageManager) {
	hc.store = store
	hc.unhealthyHostList = make(map[string]bool)
	// Generate a new ID for ourselves
	hc.GenerateCheckerId()
}

func (hc *HostCheckerManager) Start() {
	// Start loop to check if we are active instance
	if hc.Id != "" {
		go hc.CheckActivePollerLoop()
		if config.UptimeTests.Config.EnableUptimeAnalytics {
			go hc.UptimePurgeLoop()
		}
	}
}

func (hc *HostCheckerManager) GenerateCheckerId() {
	hc.Id = uuid.NewUUID().String()
}

func (hc *HostCheckerManager) CheckActivePollerLoop() {
	for {
		if hc.stopLoop {
			log.Debug("[HOST CHECK MANAGER] Stopping uptime tests")
			break
		}

		// If I'm polling, lets start the loop
		if hc.AmIPolling() {
			if !hc.pollerStarted {
				log.Debug("[HOST CHECK MANAGER] Starting Poller")
				hc.pollerStarted = true
				go hc.StartPoller()
			}
		} else {
			log.Debug("[HOST CHECK MANAGER] New master found, stopping uptime tests")
			if hc.pollerStarted {
				go hc.StopPoller()
				hc.pollerStarted = false
			}
		}

		time.Sleep(10 * time.Second)
	}
}

func (hc *HostCheckerManager) UptimePurgeLoop() {
	if config.AnalyticsConfig.PurgeDelay == -1 {
		log.Warning("Analytics purge turned off, you are responsible for Redis storage maintenance.")
		return
	}
	log.Debug("[HOST CHECK MANAGER] Started analytics purge loop")
	for {
		if hc.pollerStarted {
			if hc.Clean != nil {
				log.Debug("[HOST CHECK MANAGER] Purging uptime analytics")
				hc.Clean.PurgeCache()
			}

		}
		time.Sleep(time.Duration(config.AnalyticsConfig.PurgeDelay) * time.Second)
	}
}

func (hc *HostCheckerManager) AmIPolling() bool {
	if hc.store == nil {
		log.Error("[HOST CHECK MANAGER] No storage instance set for uptime tests! Disabling poller...")
		return false
	}
	ActiveInstance, err := hc.store.GetKey(PollerCacheKey)
	if err != nil {
		log.Debug("[HOST CHECK MANAGER] No Primary instance found, assuming control")
		hc.store.SetKey(PollerCacheKey, hc.Id, 15)
		return true
	}

	if ActiveInstance == hc.Id {
		log.Debug("[HOST CHECK MANAGER] Primary instance set, I am master")
		hc.store.SetKey(PollerCacheKey, hc.Id, 15) // Reset TTL
		return true
	}

	log.Debug("Active Instance is: ", ActiveInstance)
	log.Debug("--- I am: ", hc.Id)

	return false
}

func (hc *HostCheckerManager) StartPoller() {

	log.Debug("---> Initialising checker")

	// If we are restarting, we want to retain the host list
	if hc.checker == nil {
		hc.checker = &HostUptimeChecker{}
	}

	hc.checker.Init(config.UptimeTests.Config.CheckerPoolSize,
		config.UptimeTests.Config.FailureTriggerSampleSize,
		config.UptimeTests.Config.TimeWait,
		hc.currentHostList,
		hc.OnHostDown,   // On failure
		hc.OnHostBackUp, // On success
		hc.OnHostReport) // All reports

	// Start the check loop
	log.Debug("---> Starting checker")
	hc.checker.Start()
	log.Debug("---> Checker started.")
}

func (hc *HostCheckerManager) StopPoller() {
	if hc.checker != nil {
		hc.checker.Stop()
	}
}

func (hc *HostCheckerManager) getHostKey(report HostHealthReport) string {
	return PoolerHostSentinelKeyPrefix + report.MetaData[UnHealthyHostMetaDataHostKey]
}

func (hc *HostCheckerManager) OnHostReport(report HostHealthReport) {
	if config.UptimeTests.Config.EnableUptimeAnalytics {
		go hc.RecordUptimeAnalytics(report)
	}
}

func (hc *HostCheckerManager) OnHostDown(report HostHealthReport) {
	log.Debug("Update key: ", hc.getHostKey(report))
	hc.store.SetKey(hc.getHostKey(report), "1", int64(config.UptimeTests.Config.TimeWait))

	thisSpec, found := ApiSpecRegister[report.MetaData[UnHealthyHostMetaDataAPIKey]]
	if !found {
		log.Warning("[HOST CHECKER MANAGER] Event can't fire for API that doesn't exist")
		return
	}

	go thisSpec.FireEvent(EVENT_HOSTDOWN,
		EVENT_HostStatusMeta{
			EventMetaDefault: EventMetaDefault{Message: "Uptime test failed"},
			HostInfo:         report,
		})

	log.Warning("[HOST CHECKER MANAGER] Host is DOWN: ", report.CheckURL)
}

func (hc *HostCheckerManager) OnHostBackUp(report HostHealthReport) {
	log.Debug("Delete key: ", hc.getHostKey(report))
	hc.store.DeleteKey(hc.getHostKey(report))

	thisSpec, found := ApiSpecRegister[report.MetaData[UnHealthyHostMetaDataAPIKey]]
	if !found {
		log.Warning("[HOST CHECKER MANAGER] Event can't fire for API that doesn't exist")
		return
	}
	go thisSpec.FireEvent(EVENT_HOSTUP,
		EVENT_HostStatusMeta{
			EventMetaDefault: EventMetaDefault{Message: "Uptime test suceeded"},
			HostInfo:         report,
		})

	log.Warning("[HOST CHECKER MANAGER] Host is UP:   ", report.CheckURL)
}

func (hc *HostCheckerManager) IsHostDown(thisUrl string) bool {
	u, err := url.Parse(thisUrl)
	if err != nil {
		log.Error(err)
	}

	log.Debug("Key is: ", PoolerHostSentinelKeyPrefix+u.Host)
	_, fErr := hc.store.GetKey(PoolerHostSentinelKeyPrefix + u.Host)

	if fErr != nil {
		// Found a key, the host is down
		return true
	}

	return false
}

func (hc *HostCheckerManager) PrepareTrackingHost(checkObject tykcommon.HostCheckObject, APIID string) (HostData, error) {
	// Build the check URL:
	var thisHostData HostData
	u, err := url.Parse(checkObject.CheckURL)
	if err != nil {
		log.Error(err)
		return thisHostData, err
	}

	var bodyData string
	var bodyByteArr []byte
	var loadErr error
	if len(checkObject.Body) > 0 {
		bodyByteArr, loadErr = b64.StdEncoding.DecodeString(checkObject.Body)
		if loadErr != nil {
			log.Error("Failed to load blob data: ", loadErr)
			return thisHostData, loadErr
		}
		bodyData = string(bodyByteArr)
	}

	thisHostData = HostData{
		CheckURL: checkObject.CheckURL,
		ID:       checkObject.CheckURL,
		MetaData: make(map[string]string),
		Method:   checkObject.Method,
		Headers:  checkObject.Headers,
		Body:     bodyData,
	}

	// Add our specific metadata
	thisHostData.MetaData[UnHealthyHostMetaDataTargetKey] = checkObject.CheckURL
	thisHostData.MetaData[UnHealthyHostMetaDataAPIKey] = APIID
	thisHostData.MetaData[UnHealthyHostMetaDataHostKey] = u.Host

	return thisHostData, nil
}

func (hc *HostCheckerManager) UpdateTrackingList(hd []HostData) {
	log.Debug("--- Setting tracking list up")
	newHostList := make(map[string]HostData)
	for _, host := range hd {
		newHostList[host.CheckURL] = host
	}

	hc.currentHostList = newHostList
	if hc.checker != nil {
		log.Debug("Reset initiated")
		hc.checker.ResetList(&newHostList)
	}
}

// RecordHit will store an AnalyticsRecord in Redis
func (hc HostCheckerManager) RecordUptimeAnalytics(thisReport HostHealthReport) error {
	// If we are obfuscating API Keys, store the hashed representation (config check handled in hashing function)

	thisSpec, found := ApiSpecRegister[thisReport.MetaData[UnHealthyHostMetaDataAPIKey]]
	thisOrg := ""
	if found {
		thisOrg = thisSpec.OrgID
	}

	t := time.Now()

	var serverError bool
	if thisReport.ResponseCode > 200 {
		serverError = true
	}
	newAnalyticsRecord := UptimeReportData{
		URL:          thisReport.CheckURL,
		RequestTime:  int64(thisReport.Latency),
		ResponseCode: thisReport.ResponseCode,
		TCPError:     thisReport.IsTCPError,
		ServerError:  serverError,
		Day:          t.Day(),
		Month:        t.Month(),
		Year:         t.Year(),
		Hour:         t.Hour(),
		TimeStamp:    t,
		APIID:        thisReport.MetaData[UnHealthyHostMetaDataAPIKey],
		OrgID:        thisOrg,
	}

	newAnalyticsRecord.SetExpiry(thisSpec.UptimeTests.Config.ExpireUptimeAnalyticsAfter)

	encoded, err := msgpack.Marshal(newAnalyticsRecord)

	if err != nil {
		log.Error("Error encoding uptime data:", err)
		return err
	}

	hc.store.AppendToSet(UptimeAnalytics_KEYNAME, string(encoded))
	return nil
}

func InitHostCheckManager(store *RedisClusterStorageManager, purger Purger) {
	GlobalHostChecker = HostCheckerManager{}
	GlobalHostChecker.Clean = purger
	GlobalHostChecker.Init(store)
	GlobalHostChecker.Start()
}

func SetCheckerHostList() {
	log.Info("Loading uptime tests:")
	hostList := []HostData{}
	for _, spec := range ApiSpecRegister {
		for _, checkItem := range spec.UptimeTests.CheckList {
			newHostDoc, hdGenErr := GlobalHostChecker.PrepareTrackingHost(checkItem, spec.APIID)
			if hdGenErr == nil {
				hostList = append(hostList, newHostDoc)
				log.Info("---> Adding uptime test: ", checkItem.CheckURL)
			} else {
				log.Warning("---> Adding uptime test failed: ", checkItem.CheckURL)
				log.Warning("--------> Error was: ", hdGenErr)
			}

		}
	}

	GlobalHostChecker.UpdateTrackingList(hostList)
}

/*

## TEST CONFIGURATION

uptime_tests: {
    check_list: [
      {
        "url": "http://google.com:3000/"
      },
      {
        "url": "http://posttestserver.com/post.php?dir=tyk-checker-target-test&beep=boop",
        "method": "POST",
        "headers": {
          "this": "that",
          "more": "beans"
        },
        "body": "VEhJUyBJUyBBIEJPRFkgT0JKRUNUIFRFWFQNCg0KTW9yZSBzdHVmZiBoZXJl"
      }
    ]
  },

*/
