package env

import (
	"github.com/appc/spec/discovery"
	"github.com/appc/spec/schema"
	cntspec "github.com/blablacar/cnt/spec"
	"github.com/blablacar/green-garden/spec"
	"github.com/blablacar/green-garden/utils"
	"io/ioutil"
	"net/http"
	"strings"
)

func (s Service) Generate(sources []string) {
	s.log.Debug("Generating units")

	tmpl, err := s.loadUnitTemplate()
	if err != nil {
		s.log.WithError(err).Error("Cannot load units template")
		return
	}

	if len(s.manifest.Nodes) == 0 {
		s.log.Error("No node to process in manifest")
		return
	}

	nodes := s.manifest.Nodes
	if s.manifest.Nodes[0][spec.NODE_HOSTNAME].(string) == "*" {
		if len(s.manifest.Nodes) > 1 {
			s.log.Error("You cannot mix all nodes with single node. Yet ?")
			return
		}

		newNodes := *new([]map[string]interface{})
		machines := s.env.ListMachineNames()
		for _, machine := range machines {
			node := utils.CopyMap(s.manifest.Nodes[0])
			node[spec.NODE_HOSTNAME] = machine
			newNodes = append(newNodes, node)
		}

		nodes = newNodes
	}

	acis, err := s.prepareAciList(sources)
	if err != nil {
		s.log.WithError(err).Error("Cannot prepare aci list")
		return
	}

	for i, node := range nodes {
		hostname := node[spec.NODE_HOSTNAME].(string)
		if hostname == "" {
			s.log.WithField("index", i).Error("hostname is mandatory in node informations")
		}
		unit := s.LoadUnit(hostname)
		unit.Generate(node, tmpl, acis, s.attributes)
	}
}

func (s Service) podManifestToMap(result map[string][]cntspec.ACFullname, contents []byte) error {
	pod := schema.BlankPodManifest()
	err := pod.UnmarshalJSON(contents)
	if err != nil {
		return err
	}

	var podname string
	var acis []cntspec.ACFullname
	for i, podAci := range pod.Apps {
		version, _ := podAci.Image.Labels.Get("version")
		fullname := cntspec.NewACFullName(podAci.Image.Name.String() + ":" + version)
		if i == 0 {
			nameSplit := strings.SplitN(fullname.ShortName(), "_", 2)
			podname = fullname.DomainName() + "/" + nameSplit[0]
		}

		//		resolved, err := fullname.FullyResolved() // TODO should not be resolved for local test ??
		//		if err != nil {
		//			logrus.WithError(err).Fatal("Cannot fully resolve ACI")
		//		}
		acis = append(acis, *fullname)
	}

	result[podname] = acis
	return nil
}

func (s Service) aciManifestToMap(result map[string][]cntspec.ACFullname, contents []byte) error {
	aci := schema.BlankImageManifest()
	err := aci.UnmarshalJSON(contents)
	if err != nil {
		return err
	}

	version, _ := aci.Labels.Get("version")
	fullname := cntspec.NewACFullName(aci.Name.String() + ":" + version)
	result[fullname.Name()] = []cntspec.ACFullname{*fullname}
	return nil
}

func (s Service) sources(sources []string) map[string][]cntspec.ACFullname {
	res := make(map[string][]cntspec.ACFullname)
	for _, source := range sources {
		content, err := ioutil.ReadFile(source)
		if err != nil {
			s.log.WithError(err).Warn("Cannot read source file")
			continue
		}
		if err := s.aciManifestToMap(res, content); err != nil {
			if err2 := s.podManifestToMap(res, content); err2 != nil {
				s.log.WithError(err).WithField("error2", err2).Error("Cannot read manifest file as aci nor pod")
			}
		}
	}
	return res
}

func (s Service) discoverPod(name cntspec.ACFullname) []cntspec.ACFullname {
	logAci := s.log.WithField("pod", name)

	app, err := discovery.NewAppFromString(name.String())
	if app.Labels["os"] == "" {
		app.Labels["os"] = "linux"
	}
	if app.Labels["arch"] == "" {
		app.Labels["arch"] = "amd64"
	}

	endpoint, _, err := discovery.DiscoverEndpoints(*app, nil, false)
	if err != nil {
		logAci.WithError(err).Fatal("pod discovery failed")
	}

	if len(endpoint.ACIEndpoints) == 0 {
		s.log.Panic("Discovery does not contain a url")
	}
	url := endpoint.ACIEndpoints[0].ACI
	url = strings.Replace(url, "=aci", "=pod", 1) // TODO this is nexus specific

	logUrl := logAci.WithField("url", url)
	response, err := http.Get(url)
	if err != nil {
		logUrl.WithError(err).Fatal("Cannot get pod manifest content")
		return nil
	} else {
		if response.StatusCode != 200 {
			logUrl.WithField("status_code", response.StatusCode).WithField("status_message", response.Status).
				Fatal("Receive response error for discovery")
		}
		defer response.Body.Close()
		content, err := ioutil.ReadAll(response.Body)
		if err != nil {
			logUrl.WithError(err).Fatal("Cannot read pod manifest content")
		}
		tmpMap := make(map[string][]cntspec.ACFullname, 1)
		if err := s.podManifestToMap(tmpMap, content); err != nil {
			logUrl.WithError(err).Fatal("Cannot read pod content")
		}
		acis := tmpMap[name.Name()]
		if acis == nil {
			logUrl.Fatal("Discovered pod name does not match requested")
		}
		return acis
	}
}

func (s Service) prepareAciList(sources []string) (string, error) {
	if len(s.manifest.Containers) == 0 {
		return "", nil
	}

	override := s.sources(sources)
	s.log.WithField("data", override).Debug("Local resolved sources")

	var acis string
	for _, aci := range s.manifest.Containers {
		containerLog := s.log.WithField("container", aci.String())
		containerLog.Debug("Processing container")
		if strings.HasPrefix(aci.ShortName(), "pod-") {
			var podAcis []cntspec.ACFullname
			if override[aci.Name()] != nil {
				containerLog.Debug("Using local source to resolve")
				podAcis = override[aci.Name()]
			} else {
				containerLog.Debug("Using remote source to resolve")
				podAcis = s.discoverPod(aci)
			}
			for _, aci := range podAcis {
				acis += aci.String() + " "
			}
		} else {
			var taci cntspec.ACFullname
			if override[aci.Name()] != nil {
				containerLog.Debug("Using local source to resolve")
				taci = override[aci.Name()][0]
			} else {
				containerLog.Debug("Using remote source to resolve")
				aciTmp, err := aci.FullyResolved()
				taci = *aciTmp
				if err != nil {
					containerLog.Fatal("Cannot resolve aci")
					return "", err
				}
			}
			acis += taci.String() + " "
		}
	}
	if acis == "" {
		s.log.Error("Aci list is empty after discovery")
	}
	return acis, nil
}
