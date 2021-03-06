package localbroker

import (
	"context"
	"reflect"

	"path"

	"encoding/json"

	"fmt"

	"path/filepath"

	"sync"

	"os"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/goshims/ioutilshim"
	"code.cloudfoundry.org/goshims/osshim"
	"github.com/pivotal-cf/brokerapi"
)

const (
	PermissionVolumeMount = brokerapi.RequiredPermission("volume_mount")
	DefaultContainerPath  = "/var/vcap/data"
)

type staticState struct {
	ServiceName string `json:"ServiceName"`
	ServiceId   string `json:"ServiceId"`
	PlanName    string `json:"PlanName"`
	PlanId      string `json:"PlanId"`
	PlanDesc    string `json:"PlanDesc"`
}

type dynamicState struct {
	InstanceMap map[string]brokerapi.ProvisionDetails
	BindingMap  map[string]brokerapi.BindDetails
}

type lock interface {
	Lock()
	Unlock()
}

type broker struct {
	logger      lager.Logger
	dataDir     string
	os          osshim.Os
	ioutil      ioutilshim.Ioutil
	mutex       lock

	static  staticState
	dynamic dynamicState
}

func New(
	logger lager.Logger,
	serviceName, serviceId, planName, planId, planDesc, dataDir string,
	os osshim.Os, ioutil ioutilshim.Ioutil,
) *broker {

	theBroker := broker{
		logger:      logger,
		dataDir:     dataDir,
		os:          os,
		ioutil:      ioutil,
		mutex:       &sync.Mutex{},
		static: staticState{
			ServiceName: serviceName,
			ServiceId:   serviceId,
			PlanName:    planName,
			PlanId:      planId,
			PlanDesc:    planDesc,
		},
		dynamic: dynamicState{
			InstanceMap: map[string]brokerapi.ProvisionDetails{},
			BindingMap:  map[string]brokerapi.BindDetails{},
		},
	}

	theBroker.restoreDynamicState()

	return &theBroker
}

func (b *broker) Services(_ context.Context) []brokerapi.Service {
	logger := b.logger.Session("services")
	logger.Info("start")
	defer logger.Info("end")

	return []brokerapi.Service{{
		ID:            b.static.ServiceId,
		Name:          b.static.ServiceName,
		Description:   "Local service docs: https://github.com/cloudfoundry-incubator/local-volume-release/",
		Bindable:      true,
		PlanUpdatable: false,
		Tags:          []string{"local"},
		Requires:      []brokerapi.RequiredPermission{PermissionVolumeMount},

		Plans: []brokerapi.ServicePlan{{
			Name:        b.static.PlanName,
			ID:          b.static.PlanId,
			Description: b.static.PlanDesc,
		}},
	}}
}

func (b *broker) Provision(context context.Context, instanceID string, details brokerapi.ProvisionDetails, asyncAllowed bool) (brokerapi.ProvisionedServiceSpec, error) {
	logger := b.logger.Session("provision")
	logger.Info("start")
	defer logger.Info("end")

	b.mutex.Lock()
	defer b.mutex.Unlock()

	defer b.serialize(b.dynamic)

	if b.instanceConflicts(details, instanceID) {
		logger.Error("instance-already-exists", brokerapi.ErrInstanceAlreadyExists)
		return brokerapi.ProvisionedServiceSpec{}, brokerapi.ErrInstanceAlreadyExists
	}

	b.dynamic.InstanceMap[instanceID] = details

	return brokerapi.ProvisionedServiceSpec{}, nil
}

func (b *broker) Deprovision(context context.Context, instanceID string, details brokerapi.DeprovisionDetails, asyncAllowed bool) (brokerapi.DeprovisionServiceSpec, error) {
	logger := b.logger.Session("deprovision")
	logger.Info("start")
	defer logger.Info("end")

	b.mutex.Lock()
	defer b.mutex.Unlock()

	defer b.serialize(b.dynamic)

	if _, ok := b.dynamic.InstanceMap[instanceID]; !ok {
		return brokerapi.DeprovisionServiceSpec{}, brokerapi.ErrInstanceDoesNotExist
	}

	delete(b.dynamic.InstanceMap, instanceID)

	return brokerapi.DeprovisionServiceSpec{}, nil
}

func (b *broker) Bind(_ context.Context, instanceID string, bindingID string, details brokerapi.BindDetails) (brokerapi.Binding, error) {
	logger := b.logger.Session("bind")
	logger.Info("start")
	defer logger.Info("end")

	b.mutex.Lock()
	defer b.mutex.Unlock()

	defer b.serialize(b.dynamic)

	if _, ok := b.dynamic.InstanceMap[instanceID]; !ok {
		return brokerapi.Binding{}, brokerapi.ErrInstanceDoesNotExist
	}

	if details.AppGUID == "" {
		return brokerapi.Binding{}, brokerapi.ErrAppGuidNotProvided
	}

	mode, err := evaluateMode(details.Parameters)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	if b.bindingConflicts(bindingID, details) {
		return brokerapi.Binding{}, brokerapi.ErrBindingAlreadyExists
	}

	b.dynamic.BindingMap[bindingID] = details

	return brokerapi.Binding{
		Credentials: struct{}{}, // if nil, cloud controller chokes on response
		VolumeMounts: []brokerapi.VolumeMount{{
			ContainerDir: evaluateContainerPath(details.Parameters, instanceID),
			Mode:         mode,
			Driver:       "localdriver",
			DeviceType:   "shared",
			Device: brokerapi.SharedDevice{
				VolumeId: instanceID,
			},
		}},
	}, nil
}

func (b *broker) Unbind(_ context.Context, instanceID string, bindingID string, details brokerapi.UnbindDetails) error {
	logger := b.logger.Session("unbind")
	logger.Info("start")
	defer logger.Info("end")

	b.mutex.Lock()
	defer b.mutex.Unlock()

	defer b.serialize(b.dynamic)

	if _, ok := b.dynamic.InstanceMap[instanceID]; !ok {
		return brokerapi.ErrInstanceDoesNotExist
	}

	if _, ok := b.dynamic.BindingMap[bindingID]; !ok {
		return brokerapi.ErrBindingDoesNotExist
	}

	delete(b.dynamic.BindingMap, bindingID)

	return nil
}

func (b *broker) Update(_ context.Context, instanceID string, details brokerapi.UpdateDetails, asyncAllowed bool) (brokerapi.UpdateServiceSpec, error) {
	panic("not implemented")
}

func (b *broker) LastOperation(_ context.Context, instanceID string, operationData string) (brokerapi.LastOperation, error) {
	panic("not implemented")
}

func (b *broker) instanceConflicts(details brokerapi.ProvisionDetails, instanceID string) bool {
	if existing, ok := b.dynamic.InstanceMap[instanceID]; ok {
		if !reflect.DeepEqual(details, existing) {
			return true
		}
	}
	return false
}

func evaluateContainerPath(parameters map[string]interface{}, volId string) string {
	if containerPath, ok := parameters["mount"]; ok && containerPath != "" {
		return containerPath.(string)
	}

	return path.Join(DefaultContainerPath, volId)
}

func evaluateMode(parameters map[string]interface{}) (string, error) {
	if ro, ok := parameters["readonly"]; ok {
		switch ro := ro.(type) {
		case bool:
			return readOnlyToMode(ro), nil
		default:
			return "", brokerapi.ErrRawParamsInvalid
		}
	}
	return "rw", nil
}

func readOnlyToMode(ro bool) string {
	if ro {
		return "r"
	}
	return "rw"
}

func (b *broker) bindingConflicts(bindingID string, details brokerapi.BindDetails) bool {
	if existing, ok := b.dynamic.BindingMap[bindingID]; ok {
		if !reflect.DeepEqual(details, existing) {
			return true
		}
	}
	return false
}

func (b *broker) serialize(state interface{}) {
	logger := b.logger.Session("serialize-state")
	logger.Info("start")
	defer logger.Info("end")

	stateFile := filepath.Join(b.dataDir, fmt.Sprintf("%s-services.json", b.static.ServiceName))

	stateData, err := json.Marshal(state)
	if err != nil {
		b.logger.Error("failed-to-marshall-state", err)
		return
	}

	err = b.ioutil.WriteFile(stateFile, stateData, os.ModePerm)
	if err != nil {
		b.logger.Error("failed-to-write-state-file", err, lager.Data{"stateFile": stateFile})
		return
	}

	logger.Info("state-saved", lager.Data{"state-file": stateFile})
}

func (b *broker) restoreDynamicState() {
	logger := b.logger.Session("restore-services")
	logger.Info("start")
	defer logger.Info("end")

	stateFile := filepath.Join(b.dataDir, fmt.Sprintf("%s-services.json", b.static.ServiceName))

	serviceData, err := b.ioutil.ReadFile(stateFile)
	if err != nil {
		b.logger.Error("failed-to-read-state-file", err, lager.Data{"stateFile": stateFile})
		return
	}

	dynamicState := dynamicState{}
	err = json.Unmarshal(serviceData, &dynamicState)
	if err != nil {
		b.logger.Error("failed-to-unmarshall-state", err, lager.Data{"stateFile": stateFile})
		return
	}
	logger.Info("state-restored", lager.Data{"state-file": stateFile})
	b.dynamic = dynamicState
}
