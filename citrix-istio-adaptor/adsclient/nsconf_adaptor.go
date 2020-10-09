/*
Copyright 2019 Citrix Systems, Inc
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package adsclient

import (
	"citrix-istio-adaptor/nsconfigengine"
	"container/list"
	"fmt"
	"github.com/chiradeep/go-nitro/config/basic"
	"github.com/chiradeep/go-nitro/config/dns"
	"github.com/chiradeep/go-nitro/config/lb"
	"github.com/chiradeep/go-nitro/config/network"
	"github.com/chiradeep/go-nitro/config/ns"
	"github.com/chiradeep/go-nitro/config/responder"
	"github.com/chiradeep/go-nitro/config/tm"
	"github.com/chiradeep/go-nitro/netscaler"
	"io/ioutil"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

type discoveryType int

const (
	cdsAdd discoveryType = iota
	cdsDel
	ldsAdd
	ldsDel
	edsAdd
	rdsAdd
	//MSS for setting various tcp profiles
	MSS = 1410
)

type configBlock struct {
	configType   discoveryType
	resourceName string
	resource     interface{}
}

type configAdaptor struct {
	adsServerPort     string
	client            *netscaler.NitroClient
	nsip              string
	mux               sync.Mutex
	configs           *list.List
	cdsHash           map[string]*list.Element
	edsHash           map[string]*list.Element
	ldsHash           map[string]*list.Element
	rdsHash           map[string]*list.Element
	watch             *Watcher
	quit              chan bool
	vserverIP         string
	netProfile        string
	analyticsServerIP string
}

func newConfigAdaptor(url, username, password, adsServerPort, vserverIP, netProfile, analyticsServerIP string) (*configAdaptor, error) {
	configAdaptor := new(configAdaptor)
	configAdaptor.adsServerPort = adsServerPort
	configAdaptor.vserverIP = vserverIP
	configAdaptor.netProfile = netProfile
	configAdaptor.configs = list.New()
	configAdaptor.cdsHash = make(map[string]*list.Element)
	configAdaptor.edsHash = make(map[string]*list.Element)
	configAdaptor.ldsHash = make(map[string]*list.Element)
	configAdaptor.rdsHash = make(map[string]*list.Element)
	configAdaptor.quit = make(chan bool)
	configAdaptor.analyticsServerIP = analyticsServerIP
	var err error
	configAdaptor.client, err = netscaler.NewNitroClientFromParams(netscaler.NitroParams{Url: url, Username: username, Password: password})
	if err != nil {
		return nil, err
	}
	for {
		nsip, err := configAdaptor.getNitroObject(netscaler.Nsip.Type(), map[string]string{"type": "NSIP"})
		if err == nil {
			configAdaptor.nsip = nsip["ipaddress"].(string)
			break
		}
		time.Sleep(1 * time.Second)
	}
	if strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1") {
		err = configAdaptor.sidecarBootstrapConfig()
		if err != nil {
			return nil, err
		}
	}
	err = configAdaptor.bootstrapConfig()
	if err != nil {
		return nil, err
	}
	if strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1") {
		/* Citrix ADC CPX Case Saving the initial bootstrap configuration */
		configAdaptor.client.SaveConfig()
	}
	if vserverIP == "nsip" {
		configAdaptor.vserverIP = configAdaptor.nsip
	}
	build, err := configAdaptor.client.FindResource(netscaler.Nsversion.Type(), "")
	if err != nil {
		return nil, err
	}
	err = nsconfigengine.SetNsReleaseBuild(build)
	if err != nil {
		return nil, err
	}
	return configAdaptor, nil
}

func getNameServer() (string, error) {
	b, err := ioutil.ReadFile("/etc/resolv.conf")
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile("[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+")
	match := re.FindString(string(b))
	return match, nil
}

func (confAdaptor *configAdaptor) sidecarBootstrapConfig() error {
	var err error
	var nameServer string
	err = confAdaptor.client.ClearConfig()
	if err != nil {
		return err
	}
	nameServer, err = getNameServer()
	if err != nil {
		return err
	}
	configs := []nsconfigengine.NsConfigEntity{
		{ResourceType: netscaler.Service.Type(), ResourceName: "dns_service", Resource: basic.Service{Name: "dns_service", Ip: nameServer, Port: 53, Servicetype: "DNS", Healthmonitor: "no"}},
		{ResourceType: netscaler.Lbvserver.Type(), ResourceName: "dns_vserver", Resource: lb.Lbvserver{Name: "dns_vserver", Servicetype: "DNS"}},
		{ResourceType: netscaler.Lbvserver_service_binding.Type(), ResourceName: "dns_vserver", Resource: lb.Lbvserverservicebinding{Name: "dns_vserver", Servicename: "dns_service"}},
		{ResourceType: netscaler.Dnsnameserver.Type(), ResourceName: "dns_vserver", Resource: &dns.Dnsnameserver{Dnsvservername: "dns_vserver"}},
		{ResourceType: netscaler.Nsacl.Type(), ResourceName: "allowpromexp", Resource: ns.Nsacl{Aclname: "allowpromexp", Aclaction: "ALLOW", Protocol: "TCP", Destport: true, Destportval: "8888", Priority: 65536}},
		{ResourceType: netscaler.Nsacl.Type(), ResourceName: "denyall", Resource: ns.Nsacl{Aclname: "denyall", Aclaction: "DENY", Priority: 100000}},
		{ResourceType: netscaler.Nsacls.Type(), ResourceName: "", Resource: ns.Nsacls{}, Operation: "apply"},
	}
	if confAdaptor.analyticsServerIP != "" {
		// TODO - allowing all connection to ADS server;s port. Must find a way to allow outbound connection from adsclient and not from application server
		configs = append(configs, nsconfigengine.NsConfigEntity{ResourceType: netscaler.Nsacl.Type(), ResourceName: "allowadmserver", Resource: ns.Nsacl{Aclname: "allowadmserver", Aclaction: "ALLOW", Srcip: true, Srcipval: confAdaptor.analyticsServerIP, Priority: 65537}})
		configs = append(configs, nsconfigengine.NsConfigEntity{ResourceType: netscaler.Lbvserver.Type(), ResourceName: "drop_all_vserver", Resource: lb.Lbvserver{Name: "drop_all_vserver", Servicetype: "ANY", Ipv46: "*", Port: 65535, Listenpolicy: "(CLIENT.TCP.DSTPORT.NE(" + confAdaptor.adsServerPort + ") && CLIENT.IP.DST.NE(" + confAdaptor.analyticsServerIP + "))"}})
	} else {
		// TODO - allowing all connection to ADS server;s port. Must find a way to allow outbound connection from adsclient and not from application server
		configs = append(configs, nsconfigengine.NsConfigEntity{ResourceType: netscaler.Lbvserver.Type(), ResourceName: "drop_all_vserver", Resource: lb.Lbvserver{Name: "drop_all_vserver", Servicetype: "ANY", Ipv46: "*", Port: 65535, Listenpolicy: "CLIENT.TCP.DSTPORT.NE(" + confAdaptor.adsServerPort + ")"}})
	}
	err = nsconfigengine.NsConfigCommit(confAdaptor.client, configs)
	return err
}

func (confAdaptor *configAdaptor) bootstrapConfig() error {
	var err error
	err = confAdaptor.client.EnableFeatures([]string{"lb", "cs", "ssl", "rewrite", "responder"})
	if err != nil {
		return err
	}
	configs := []nsconfigengine.NsConfigEntity{
		{ResourceType: netscaler.Nstcpprofile.Type(), ResourceName: "nstcp_default_profile", Resource: ns.Nstcpprofile{Name: "nstcp_default_profile", Mss: MSS}, Operation: "set"},
		{ResourceType: netscaler.Nstcpprofile.Type(), ResourceName: "nstcp_internal_apps", Resource: ns.Nstcpprofile{Name: "nstcp_internal_apps", Mss: MSS}, Operation: "set"},
		{ResourceType: netscaler.Nstcpprofile.Type(), ResourceName: "nsulfd_default_profile", Resource: ns.Nstcpprofile{Name: "nsulfd_default_profile", Mss: MSS}, Operation: "set"},
		{ResourceType: netscaler.Nshttpprofile.Type(), ResourceName: "nshttp_default_profile", Resource: ns.Nshttpprofile{Name: "nshttp_default_profile", Http2: "ENABLED", Http2maxconcurrentstreams: 1000}, Operation: "set"},
		{ResourceType: netscaler.Responderaction.Type(), ResourceName: "return404", Resource: responder.Responderaction{Name: "return404", Type: "respondwith", Target: "\"HTTP/1.1 404 Not found\r\n\r\n\""}},
		{ResourceType: netscaler.Responderpolicy.Type(), ResourceName: "return404", Resource: responder.Responderpolicy{Name: "return404", Rule: "true", Action: "return404"}},
		{ResourceType: netscaler.Lbvserver.Type(), ResourceName: "ns_blackhole_http", Resource: lb.Lbvserver{Name: "ns_blackhole_http", Servicetype: "HTTP"}},
		{ResourceType: netscaler.Service.Type(), ResourceName: "ns_blackhole_http", Resource: basic.Service{Name: "ns_blackhole_http", Ip: "127.0.0.1", Port: 1, Servicetype: "HTTP", Healthmonitor: "no"}},
		{ResourceType: netscaler.Lbvserver_service_binding.Type(), ResourceName: "ns_blackhole_http", Resource: lb.Lbvserverservicebinding{Name: "ns_blackhole_http", Servicename: "ns_blackhole_http"}, IgnoreErrors: []string{"Resource already exists"}},
		{ResourceType: netscaler.Lbvserver_responderpolicy_binding.Type(), ResourceName: "ns_blackhole_http", Resource: lb.Lbvserverresponderpolicybinding{Name: "ns_blackhole_http", Policyname: "return404", Priority: 1}},
		// Dummy HTTP Vserver is added for Redirect Case
		{ResourceType: netscaler.Lbvserver.Type(), ResourceName: "ns_dummy_http", Resource: lb.Lbvserver{Name: "ns_dummy_http", Servicetype: "HTTP"}},
		{ResourceType: netscaler.Lbvserver_service_binding.Type(), ResourceName: "ns_dummy_http", Resource: lb.Lbvserverservicebinding{Name: "ns_dummy_http", Servicename: "ns_blackhole_http"}, IgnoreErrors: []string{"Resource already exists"}},
	}
	err = confAdaptor.client.EnableFeatures([]string{"aaa"})
	if err != nil {
		log.Println("[WARN] aaa feature is not enabled and JWT authentication will not work")
	} else {

		configs = append(configs, nsconfigengine.NsConfigEntity{ResourceType: netscaler.Tmsessionparameter.Type(), ResourceName: "", Resource: tm.Tmsessionparameter{Defaultauthorizationaction: "ALLOW"}, Operation: "set"})
	}
	if len(confAdaptor.netProfile) > 0 {
		netprof := nsconfigengine.NsConfigEntity{
			ResourceType: netscaler.Netprofile.Type(),
			ResourceName: confAdaptor.netProfile,
			Resource:     network.Netprofile{Name: confAdaptor.netProfile},
		}
		configs = append(configs, netprof)
	}
	err = nsconfigengine.NsConfigCommit(confAdaptor.client, configs)
	return err
}

func (confAdaptor *configAdaptor) getNitroObject(ResourceType string, filter map[string]string) (map[string]interface{}, error) {
	objs, err := confAdaptor.client.FindFilteredResourceArray(ResourceType, filter)
	if err == nil {
		if len(objs) != 1 {
			return nil, fmt.Errorf("Expected 1 object of type %s, rcvd %d objects", ResourceType, len(objs))
		}
		return objs[0], nil
	}
	return nil, err
}

func (confAdaptor *configAdaptor) getConfigMap(ct discoveryType) (map[string]*list.Element, map[string]*list.Element) {
	if ct == cdsAdd || ct == cdsDel {
		return confAdaptor.cdsHash, confAdaptor.edsHash
	}
	if ct == edsAdd {
		return confAdaptor.edsHash, nil
	}
	if ct == ldsAdd || ct == ldsDel {
		return confAdaptor.ldsHash, confAdaptor.rdsHash
	}
	if ct == rdsAdd {
		return confAdaptor.rdsHash, nil
	}
	return nil, nil
}

func (confAdaptor *configAdaptor) getConfigByName(configName string, configType discoveryType) (*configBlock, error) {
	cmap, _ := confAdaptor.getConfigMap(configType)
	if cmap == nil {
		return nil, fmt.Errorf("ConfigMap not found for %v", configType)
	}
	if e, ok := cmap[configName]; ok {
		return e.Value.(*configBlock), nil
	}
	return nil, fmt.Errorf("Config Resource not found in hash : %s", configName)
}

func (confAdaptor *configAdaptor) addConfig(config *configBlock) {
	cmap, _ := confAdaptor.getConfigMap(config.configType)
	confAdaptor.mux.Lock()
	if cmap != nil {
		if e, ok := cmap[config.resourceName]; ok {
			enew := confAdaptor.configs.InsertBefore(config, e)
			_ = confAdaptor.configs.Remove(e)
			cmap[config.resourceName] = enew
		} else {
			e := confAdaptor.configs.PushBack(config)
			cmap[config.resourceName] = e
		}
	} else {
		confAdaptor.configs.PushBack(config)
	}
	confAdaptor.mux.Unlock()
}

func (confAdaptor *configAdaptor) delConfig(config *configBlock) {
	cmap, dmap := confAdaptor.getConfigMap(config.configType)
	confAdaptor.mux.Lock()
	if e, ok := cmap[config.resourceName]; ok {
		_ = confAdaptor.configs.Remove(e)
		delete(cmap, config.resourceName)
	}
	e := confAdaptor.configs.PushBack(config)
	cmap[config.resourceName] = e

	if dmap != nil {
		if e, ok := dmap[config.resourceName]; ok {
			_ = confAdaptor.configs.Remove(e)
			delete(dmap, config.resourceName)
		}
	}
	confAdaptor.mux.Unlock()
}

func (confAdaptor *configAdaptor) startConfigAdaptor() {
	go func() {
		log.Println("[TRACE] Starting Config adaptor")
		for {
			select {
			case <-confAdaptor.quit:
				log.Println("[TRACE] Stopping Config adaptor")
				return
			default:
				confAdaptor.mux.Lock()
				var config *configBlock
				e := confAdaptor.configs.Front()
				if e != nil {
					config = confAdaptor.configs.Remove(e).(*configBlock)
					cmap, _ := confAdaptor.getConfigMap(config.configType)
					delete(cmap, config.resourceName)
				} else {
					config = nil
				}
				confAdaptor.mux.Unlock()
				if config != nil {
					var err error
					switch config.configType {
					case cdsAdd:
						err = config.resource.(*nsconfigengine.LBApi).Add(confAdaptor.client)
					case cdsDel:
						err = config.resource.(*nsconfigengine.LBApi).Delete(confAdaptor.client)
					case ldsAdd:
						err = config.resource.(*nsconfigengine.CSApi).Add(confAdaptor.client)
					case ldsDel:
						err = config.resource.(*nsconfigengine.CSApi).Delete(confAdaptor.client)
					case edsAdd:
						err = config.resource.(*nsconfigengine.ServiceGroupAPI).Add(confAdaptor.client)
					case rdsAdd:
						err = config.resource.(*nsconfigengine.CSBindingsAPI).Add(confAdaptor.client)
					}
					if err != nil {
						log.Printf("xDS application failed with error %v", err)
					}
				} else {
					time.Sleep(1 * time.Second)
				}
			}
		}
	}()
}

func (confAdaptor *configAdaptor) stopConfigAdaptor() {
	confAdaptor.quit <- true
}
