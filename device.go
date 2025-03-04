package giDevice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/electricbubble/gidevice/pkg/ipa"
	"github.com/electricbubble/gidevice/pkg/libimobiledevice"
	"github.com/electricbubble/gidevice/pkg/nskeyedarchiver"
	uuid "github.com/satori/go.uuid"
	"howett.net/plist"
)

const LockdownPort = 62078

var _ Device = (*device)(nil)

func newDevice(client *libimobiledevice.UsbmuxClient, properties DeviceProperties) *device {
	return &device{
		umClient:   client,
		properties: &properties,
	}
}

type device struct {
	umClient       *libimobiledevice.UsbmuxClient
	lockdownClient *libimobiledevice.LockdownClient

	properties *DeviceProperties

	lockdown          *lockdown
	imageMounter      ImageMounter
	screenshot        Screenshot
	simulateLocation  SimulateLocation
	installationProxy InstallationProxy
	instruments       Instruments
	afc               Afc
	houseArrest       HouseArrest
	syslogRelay       SyslogRelay
	diagnosticsRelay  DiagnosticsRelay
	crashReportMover  CrashReportMover
	pcapd             Pcapd
}

func (d *device) Properties() DeviceProperties {
	return *d.properties
}

func (d *device) NewConnect(port int, timeout ...time.Duration) (InnerConn, error) {
	newClient, err := libimobiledevice.NewUsbmuxClient(timeout...)
	if err != nil {
		return nil, err
	}

	var pkt libimobiledevice.Packet
	if pkt, err = newClient.NewPlistPacket(
		newClient.NewConnectRequest(d.properties.DeviceID, port),
	); err != nil {
		return nil, err
	}

	if err = newClient.SendPacket(pkt); err != nil {
		return nil, err
	}

	if _, err = newClient.ReceivePacket(); err != nil {
		return nil, err
	}

	return newClient.InnerConn(), err
}

func (d *device) ReadPairRecord() (pairRecord *PairRecord, err error) {
	var pkt libimobiledevice.Packet
	if pkt, err = d.umClient.NewPlistPacket(
		d.umClient.NewReadPairRecordRequest(d.properties.SerialNumber),
	); err != nil {
		return nil, err
	}

	if err = d.umClient.SendPacket(pkt); err != nil {
		return nil, err
	}

	var respPkt libimobiledevice.Packet
	if respPkt, err = d.umClient.ReceivePacket(); err != nil {
		return nil, err
	}

	var reply = struct {
		Data []byte `plist:"PairRecordData"`
	}{}
	if err = respPkt.Unmarshal(&reply); err != nil {
		return nil, err
	}

	var record PairRecord
	if _, err = plist.Unmarshal(reply.Data, &record); err != nil {
		return nil, err
	}

	pairRecord = &record
	return
}

func (d *device) SavePairRecord(pairRecord *PairRecord) (err error) {
	var data []byte
	if data, err = plist.Marshal(pairRecord, plist.XMLFormat); err != nil {
		return err
	}

	var pkt libimobiledevice.Packet
	if pkt, err = d.umClient.NewPlistPacket(
		d.umClient.NewSavePairRecordRequest(d.properties.SerialNumber, d.properties.DeviceID, data),
	); err != nil {
		return err
	}

	if err = d.umClient.SendPacket(pkt); err != nil {
		return err
	}

	if _, err = d.umClient.ReceivePacket(); err != nil {
		return err
	}

	return
}

func (d *device) DeletePairRecord() (err error) {
	var pkt libimobiledevice.Packet
	if pkt, err = d.umClient.NewPlistPacket(
		d.umClient.NewDeletePairRecordRequest(d.properties.SerialNumber),
	); err != nil {
		return err
	}

	if err = d.umClient.SendPacket(pkt); err != nil {
		return err
	}

	if _, err = d.umClient.ReceivePacket(); err != nil {
		return err
	}

	return
}

func (d *device) lockdownService() (lockdown Lockdown, err error) {
	// if d.lockdown != nil {
	// 	return d.lockdown, nil
	// }

	var innerConn InnerConn
	if innerConn, err = d.NewConnect(LockdownPort, 0); err != nil {
		return nil, err
	}
	d.lockdownClient = libimobiledevice.NewLockdownClient(innerConn)
	d.lockdown = newLockdown(d)
	_, err = d.lockdown._getProductVersion()
	lockdown = d.lockdown
	return
}

func (d *device) QueryType() (LockdownType, error) {
	if _, err := d.lockdownService(); err != nil {
		return LockdownType{}, err
	}
	return d.lockdown.QueryType()
}

func (d *device) GetValue(domain, key string) (v interface{}, err error) {
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	return d.lockdown.GetValue(domain, key)
}

func (d *device) Pair() (pairRecord *PairRecord, err error) {
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	return d.lockdown.Pair()
}

func (d *device) imageMounterService() (imageMounter ImageMounter, err error) {
	if d.imageMounter != nil {
		return d.imageMounter, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.imageMounter, err = d.lockdown.ImageMounterService(); err != nil {
		return nil, err
	}
	imageMounter = d.imageMounter
	return
}

func (d *device) Images(imgType ...string) (imageSignatures [][]byte, err error) {
	if _, err = d.imageMounterService(); err != nil {
		return nil, err
	}
	if len(imgType) == 0 {
		imgType = []string{"Developer"}
	}
	return d.imageMounter.Images(imgType[0])
}

func (d *device) MountDeveloperDiskImage(dmgPath string, signaturePath string) (err error) {
	if _, err = d.imageMounterService(); err != nil {
		return err
	}
	devImgPath := "/private/var/mobile/Media/PublicStaging/staging.dimage"
	return d.imageMounter.UploadImageAndMount("Developer", devImgPath, dmgPath, signaturePath)
}

func (d *device) screenshotService() (screenshot Screenshot, err error) {
	if d.screenshot != nil {
		return d.screenshot, nil
	}

	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.screenshot, err = d.lockdown.ScreenshotService(); err != nil {
		return nil, err
	}
	screenshot = d.screenshot
	return
}

func (d *device) Screenshot() (raw *bytes.Buffer, err error) {
	if _, err = d.screenshotService(); err != nil {
		return nil, err
	}
	return d.screenshot.Take()
}

func (d *device) simulateLocationService() (simulateLocation SimulateLocation, err error) {
	if d.simulateLocation != nil {
		return d.simulateLocation, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.simulateLocation, err = d.lockdown.SimulateLocationService(); err != nil {
		return nil, err
	}
	simulateLocation = d.simulateLocation
	return
}

func (d *device) SimulateLocationUpdate(longitude float64, latitude float64, coordinateSystem ...CoordinateSystem) (err error) {
	if _, err = d.simulateLocationService(); err != nil {
		return err
	}
	return d.simulateLocation.Update(longitude, latitude, coordinateSystem...)
}

func (d *device) SimulateLocationRecover() (err error) {
	if _, err = d.simulateLocationService(); err != nil {
		return err
	}
	return d.simulateLocation.Recover()
}

func (d *device) installationProxyService() (installationProxy InstallationProxy, err error) {
	if d.installationProxy != nil {
		return d.installationProxy, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.installationProxy, err = d.lockdown.InstallationProxyService(); err != nil {
		return nil, err
	}
	installationProxy = d.installationProxy
	return
}

func (d *device) InstallationProxyBrowse(opts ...InstallationProxyOption) (currentList []interface{}, err error) {
	if _, err = d.installationProxyService(); err != nil {
		return nil, err
	}
	return d.installationProxy.Browse(opts...)
}

func (d *device) InstallationProxyLookup(opts ...InstallationProxyOption) (lookupResult interface{}, err error) {
	if _, err = d.installationProxyService(); err != nil {
		return nil, err
	}
	return d.installationProxy.Lookup(opts...)
}

func (d *device) instrumentsService() (instruments Instruments, err error) {
	if d.instruments != nil {
		return d.instruments, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.instruments, err = d.lockdown.InstrumentsService(); err != nil {
		return nil, err
	}
	instruments = d.instruments
	return
}

func (d *device) AppLaunch(bundleID string, opts ...AppLaunchOption) (pid int, err error) {
	if _, err = d.instrumentsService(); err != nil {
		return 0, err
	}
	return d.instruments.AppLaunch(bundleID, opts...)
}

func (d *device) AppKill(pid int) (err error) {
	if _, err = d.instrumentsService(); err != nil {
		return err
	}
	return d.instruments.AppKill(pid)
}

func (d *device) AppRunningProcesses() (processes []Process, err error) {
	if _, err = d.instrumentsService(); err != nil {
		return nil, err
	}
	return d.instruments.AppRunningProcesses()
}

func (d *device) AppList(opts ...AppListOption) (apps []Application, err error) {
	if _, err = d.instrumentsService(); err != nil {
		return nil, err
	}
	return d.instruments.AppList(opts...)
}

func (d *device) DeviceInfo() (devInfo *DeviceInfo, err error) {
	if _, err = d.instrumentsService(); err != nil {
		return nil, err
	}
	return d.instruments.DeviceInfo()
}

func (d *device) testmanagerdService() (testmanagerd Testmanagerd, err error) {
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if testmanagerd, err = d.lockdown.TestmanagerdService(); err != nil {
		return nil, err
	}
	return
}

func (d *device) AfcService() (afc Afc, err error) {
	if d.afc != nil {
		return d.afc, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.afc, err = d.lockdown.AfcService(); err != nil {
		return nil, err
	}
	afc = d.afc
	return
}

func (d *device) AppInstall(ipaPath string) (err error) {
	if _, err = d.AfcService(); err != nil {
		return err
	}

	stagingPath := "PublicStaging"
	if _, err = d.afc.Stat(stagingPath); err != nil {
		if err != ErrAfcStatNotExist {
			return err
		}
		if err = d.afc.Mkdir(stagingPath); err != nil {
			return fmt.Errorf("app install: %w", err)
		}
	}

	var info map[string]interface{}
	if info, err = ipa.Info(ipaPath); err != nil {
		return err
	}
	bundleID, ok := info["CFBundleIdentifier"]
	if !ok {
		return errors.New("can't find 'CFBundleIdentifier'")
	}

	installationPath := path.Join(stagingPath, fmt.Sprintf("%s.ipa", bundleID))

	var data []byte
	if data, err = os.ReadFile(ipaPath); err != nil {
		return err
	}
	if err = d.afc.WriteFile(installationPath, data, AfcFileModeWr); err != nil {
		return err
	}

	if _, err = d.installationProxyService(); err != nil {
		return err
	}

	return d.installationProxy.Install(fmt.Sprintf("%s", bundleID), installationPath)
}

func (d *device) AppUninstall(bundleID string) (err error) {
	if _, err = d.installationProxyService(); err != nil {
		return err
	}

	return d.installationProxy.Uninstall(bundleID)
}

func (d *device) HouseArrestService() (houseArrest HouseArrest, err error) {
	if d.houseArrest != nil {
		return d.houseArrest, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.houseArrest, err = d.lockdown.HouseArrestService(); err != nil {
		return nil, err
	}
	houseArrest = d.houseArrest
	return
}

func (d *device) syslogRelayService() (syslogRelay SyslogRelay, err error) {
	if d.syslogRelay != nil {
		return d.syslogRelay, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.syslogRelay, err = d.lockdown.SyslogRelayService(); err != nil {
		return nil, err
	}
	syslogRelay = d.syslogRelay
	return
}

func (d *device) Syslog() (lines <-chan string, err error) {
	if _, err = d.syslogRelayService(); err != nil {
		return nil, err
	}
	return d.syslogRelay.Lines(), nil
}

func (d *device) SyslogStop() {
	if d.syslogRelay == nil {
		return
	}
	d.syslogRelay.Stop()
}

func (d *device) Reboot() (err error) {
	if _, err = d.lockdownService(); err != nil {
		return
	}
	if d.diagnosticsRelay, err = d.lockdown.DiagnosticsRelayService(); err != nil {
		return
	}
	if err = d.diagnosticsRelay.Reboot(); err != nil {
		return
	}
	return
}

func (d *device) Shutdown() (err error) {
	if _, err = d.lockdownService(); err != nil {
		return
	}
	if d.diagnosticsRelay, err = d.lockdown.DiagnosticsRelayService(); err != nil {
		return
	}
	if err = d.diagnosticsRelay.Shutdown(); err != nil {
		return
	}
	return
}

func (d *device) PcapdService() (pcapd Pcapd, err error) {
	// if d.pcapd != nil {
	// 	return d.pcapd, nil
	// }
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.pcapd, err = d.lockdown.PcapdService(); err != nil {
		return nil, err
	}
	pcapd = d.pcapd
	return
}

func (d *device) Pcap() (lines <-chan []byte, err error) {
	if _, err = d.PcapdService(); err != nil {
		return nil, err
	}
	return d.pcapd.Packet(), nil
}

func (d *device) PcapStop() {
	if d.pcapd == nil {
		return
	}
	d.pcapd.Stop()
}

func (d *device) crashReportMoverService() (crashReportMover CrashReportMover, err error) {
	if d.crashReportMover != nil {
		return d.crashReportMover, nil
	}
	if _, err = d.lockdownService(); err != nil {
		return nil, err
	}
	if d.crashReportMover, err = d.lockdown.CrashReportMoverService(); err != nil {
		return nil, err
	}
	crashReportMover = d.crashReportMover
	return
}

func (d *device) MoveCrashReport(hostDir string, opts ...CrashReportMoverOption) (err error) {
	if _, err = d.crashReportMoverService(); err != nil {
		return err
	}
	return d.crashReportMover.Move(hostDir, opts...)
}

func (d *device) XCTest(bundleID string, opts ...XCTestOption) (out <-chan string, cancel context.CancelFunc, err error) {
	xcTestOpt := defaultXCTestOption()
	for _, fn := range opts {
		fn(xcTestOpt)
	}

	ctx, cancelFunc := context.WithCancel(context.TODO())
	_out := make(chan string)

	xcodeVersion := uint64(30)

	var tmSrv1 Testmanagerd
	if tmSrv1, err = d.testmanagerdService(); err != nil {
		return _out, cancelFunc, err
	}

	var xcTestManager1 XCTestManagerDaemon
	if xcTestManager1, err = tmSrv1.newXCTestManagerDaemon(); err != nil {
		return _out, cancelFunc, err
	}

	var version []int
	if version, err = d.lockdown._getProductVersion(); err != nil {
		return _out, cancelFunc, err
	}

	if DeviceVersion(version...) >= DeviceVersion(11, 0, 0) {
		if err = xcTestManager1.initiateControlSession(xcodeVersion); err != nil {
			return _out, cancelFunc, err
		}
	}

	var tmSrv2 Testmanagerd
	if tmSrv2, err = d.testmanagerdService(); err != nil {
		return _out, cancelFunc, err
	}

	var xcTestManager2 XCTestManagerDaemon
	if xcTestManager2, err = tmSrv2.newXCTestManagerDaemon(); err != nil {
		return _out, cancelFunc, err
	}

	xcTestManager2.registerCallback("_XCT_logDebugMessage:", func(m libimobiledevice.DTXMessageResult) {
		// more information ( each operation )
		// fmt.Println("###### xcTestManager2 ### -->", m)
		if strings.Contains(fmt.Sprintf("%s", m), "Received test runner ready reply with error: (null)") {
			// fmt.Println("###### xcTestManager2 ### -->", fmt.Sprintf("%v", m.Aux[0]))
			time.Sleep(time.Second)
			if err = xcTestManager2.startExecutingTestPlan(xcodeVersion); err != nil {
				debugLog(fmt.Sprintf("startExecutingTestPlan %d: %s", xcodeVersion, err))
				return
			}
		}
	})
	xcTestManager2.registerCallback("_Golang-iDevice_Unregistered", func(m libimobiledevice.DTXMessageResult) {
		// more information
		//  _XCT_testRunnerReadyWithCapabilities:
		//  _XCT_didBeginExecutingTestPlan
		//  _XCT_didBeginInitializingForUITesting
		//  _XCT_testSuite:didStartAt:
		//  _XCT_testCase:method:willStartActivity:
		//  _XCT_testCase:method:didFinishActivity:
		//  _XCT_testCaseDidStartForTestClass:method:
		// fmt.Println("###### xcTestManager2 ### _Unregistered -->", m)
	})

	sessionId := uuid.NewV4()
	if err = xcTestManager2.initiateSession(xcodeVersion, nskeyedarchiver.NewNSUUID(sessionId.Bytes())); err != nil {
		return _out, cancelFunc, err
	}

	if _, err = d.installationProxyService(); err != nil {
		return _out, cancelFunc, err
	}

	var vResult interface{}
	if vResult, err = d.installationProxy.Lookup(WithBundleIDs(bundleID)); err != nil {
		return _out, cancelFunc, err
	}

	lookupResult := vResult.(map[string]interface{})
	lookupResult = lookupResult[bundleID].(map[string]interface{})
	appContainer := lookupResult["Container"].(string)
	appPath := lookupResult["Path"].(string)

	var pathXCTestCfg string
	if pathXCTestCfg, err = d._uploadXCTestConfiguration(bundleID, sessionId, lookupResult); err != nil {
		return _out, cancelFunc, err
	}

	if _, err = d.instrumentsService(); err != nil {
		return _out, cancelFunc, err
	}

	if err = d.instruments.appProcess(bundleID); err != nil {
		return _out, cancelFunc, err
	}

	pathXCTestConfiguration := appContainer + pathXCTestCfg

	appEnv := map[string]interface {
	}{
		"CA_ASSERT_MAIN_THREAD_TRANSACTIONS": "0",
		"CA_DEBUG_TRANSACTIONS":              "0",
		"DYLD_FRAMEWORK_PATH":                appPath + "/Frameworks:",
		"DYLD_LIBRARY_PATH":                  appPath + "/Frameworks",
		"NSUnbufferedIO":                     "YES",
		"SQLITE_ENABLE_THREAD_ASSERTIONS":    "1",
		"WDA_PRODUCT_BUNDLE_IDENTIFIER":      "",
		"XCTestConfigurationFilePath":        pathXCTestConfiguration, // Running tests with active test configuration:
		// "XCTestBundlePath":        fmt.Sprintf("%s/PlugIns/%s.xctest", appPath, name), // !!! ERROR
		// "XCTestSessionIdentifier": sessionId.String(), // !!! ERROR
		// "XCTestSessionIdentifier":  "",
		"XCODE_DBG_XPC_EXCLUSIONS": "com.apple.dt.xctestSymbolicator",
		"MJPEG_SERVER_PORT":        "",
		"USE_PORT":                 "",
		"LLVM_PROFILE_FILE":        appContainer + "/tmp/%p.profraw",
	}
	if DeviceVersion(version...) >= DeviceVersion(11, 0, 0) {
		appEnv["DYLD_INSERT_LIBRARIES"] = "/Developer/usr/lib/libMainThreadChecker.dylib"
		appEnv["OS_ACTIVITY_DT_MODE"] = "YES"
	}
	appArgs := []interface{}{
		"-NSTreatUnknownArgumentsAsOpen", "NO",
		"-ApplePersistenceIgnoreState", "YES",
	}
	appOpt := map[string]interface{}{
		"StartSuspendedKey": uint64(0),
	}
	if DeviceVersion(version...) >= DeviceVersion(12, 0, 0) {
		appOpt["ActivateSuspended"] = uint64(1)
	}

	if len(xcTestOpt.appEnv) != 0 {
		for k, v := range xcTestOpt.appEnv {
			appEnv[k] = v
		}
	}

	if len(xcTestOpt.appOpt) != 0 {
		for k, v := range xcTestOpt.appEnv {
			appOpt[k] = v
		}
	}

	d.instruments.registerCallback("outputReceived:fromProcess:atTime:", func(m libimobiledevice.DTXMessageResult) {
		// fmt.Println("###### instruments ### -->", m.Aux[0])
		_out <- fmt.Sprintf("%s", m.Aux[0])
	})

	var pid int
	if pid, err = d.instruments.AppLaunch(bundleID,
		WithAppPath(appPath),
		WithEnvironment(appEnv),
		WithArguments(appArgs),
		WithOptions(appOpt),
		WithKillExisting(true),
	); err != nil {
		return _out, cancelFunc, err
	}

	if err = d.instruments.startObserving(pid); err != nil {
		return _out, cancelFunc, err
	}

	if DeviceVersion(version...) >= DeviceVersion(12, 0, 0) {
		err = xcTestManager1.authorizeTestSession(pid)
	} else if DeviceVersion(version...) <= DeviceVersion(9, 0, 0) {
		err = xcTestManager1.initiateControlSessionForTestProcessID(pid)
	} else {
		err = xcTestManager1.initiateControlSessionForTestProcessIDProtocolVersion(pid, xcodeVersion)
	}
	if err != nil {
		return _out, cancelFunc, err
	}

	go func() {
		d.instruments.registerCallback("_Golang-iDevice_Over", func(_ libimobiledevice.DTXMessageResult) {
			cancelFunc()
		})

		<-ctx.Done()
		tmSrv1.close()
		tmSrv2.close()
		xcTestManager1.close()
		xcTestManager2.close()
		if _err := d.AppKill(pid); _err != nil {
			debugLog(fmt.Sprintf("xctest kill: %d", pid))
		}
		//time.Sleep(time.Second)
		close(_out)
		return
	}()

	return _out, cancelFunc, err
}

func (d *device) _uploadXCTestConfiguration(bundleID string, sessionId uuid.UUID, lookupResult map[string]interface{}) (pathXCTestCfg string, err error) {
	if _, err = d.HouseArrestService(); err != nil {
		return "", err
	}

	var appAfc Afc
	if appAfc, err = d.houseArrest.Container(bundleID); err != nil {
		return "", err
	}

	appTmpFilenames, err := appAfc.ReadDir("/tmp")
	if err != nil {
		return "", err
	}

	for _, tName := range appTmpFilenames {
		if strings.HasSuffix(tName, ".xctestconfiguration") {
			if _err := appAfc.Remove(fmt.Sprintf("/tmp/%s", tName)); _err != nil {
				debugLog(fmt.Sprintf("remove /tmp/%s: %s", tName, err))
				continue
			}
		}
	}

	nameExec := lookupResult["CFBundleExecutable"].(string)
	name := nameExec[:len(nameExec)-len("-Runner")]
	appPath := lookupResult["Path"].(string)

	pathXCTestCfg = fmt.Sprintf("/tmp/%s-%s.xctestconfiguration", name, strings.ToUpper(sessionId.String()))

	var content []byte
	if content, err = nskeyedarchiver.Marshal(
		nskeyedarchiver.NewXCTestConfiguration(
			nskeyedarchiver.NewNSUUID(sessionId.Bytes()),
			nskeyedarchiver.NewNSURL(fmt.Sprintf("%s/PlugIns/%s.xctest", appPath, name)),
			bundleID,
			appPath,
		),
	); err != nil {
		return "", err
	}

	if err = appAfc.WriteFile(pathXCTestCfg, content, AfcFileModeWr); err != nil {
		return "", err
	}

	return
}
