// Copyright (c) 2021 Terminus, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package runtime 应用实例相关操作
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"

	"bou.ke/monkey"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"

	"github.com/erda-project/erda-proto-go/core/dicehub/release/pb"
	orgpb "github.com/erda-project/erda-proto-go/core/org/pb"
	"github.com/erda-project/erda/apistructs"
	"github.com/erda-project/erda/bundle"
	"github.com/erda-project/erda/internal/core/org"
	"github.com/erda-project/erda/internal/pkg/mock"
	"github.com/erda-project/erda/internal/tools/orchestrator/dbclient"
	"github.com/erda-project/erda/internal/tools/orchestrator/scheduler/impl/clusterinfo"
	"github.com/erda-project/erda/internal/tools/orchestrator/services/addon"
	"github.com/erda-project/erda/pkg/database/dbengine"
	"github.com/erda-project/erda/pkg/parser/diceyml"
)

func TestModifyStatusIfNotForDisplay(t *testing.T) {
	runtime := apistructs.RuntimeInspectDTO{
		Status: "Unknown",
		Services: map[string]*apistructs.RuntimeInspectServiceDTO{
			"test": {
				Status: "Stopped",
			},
		},
	}
	updateStatusToDisplay(&runtime)
	assert.Equal(t, "Unknown", runtime.Status)
	for _, s := range runtime.Services {
		assert.Equal(t, "Stopped", s.Status)
	}
}

func TestFillRuntimeDataWithServiceGroup(t *testing.T) {
	var (
		data          apistructs.RuntimeInspectDTO
		targetService diceyml.Services
		targetJob     diceyml.Jobs
		sg            apistructs.ServiceGroup
		domainMap     = make(map[string][]string, 0)
		status        string
	)

	fakeData := `{"id":1,"name":"develop","serviceGroupName":"1f1a1k1e11","serviceGroupNamespace":"services","source":"PIPELINE","status":"Healthy","deployStatus":"CANCELED","deleteStatus":"","releaseId":"11f1a1k1e11111111111111111111111","clusterId":1,"clusterName":"fake-cluster","clusterType":"k8s","resources":{"cpu":0.6,"mem":3072,"disk":0},"extra":{"applicationId":1,"buildId":0,"workspace":"DEV"},"projectID":1,"services":{"fake-service":{"status":"Healthy","deployments":{"replicas":1},"resources":{"cpu":0.3,"mem":1536,"disk":0},"envs":{"fakeEnv":"1"},"addrs":["fake-service.services--1f1a1k1e11.svc.cluster.local:8060"],"expose":["http://fake-service-dev-1-app.dev.fake.io"],"errors":null}},"lastMessage":{},"timeCreated":"2021-06-11T16:40:49+08:00","createdAt":"2021-06-11T16:40:49+08:00","updatedAt":"2021-06-17T13:53:49+08:00","errors":null}`
	err := json.Unmarshal([]byte(fakeData), &data)
	assert.NoError(t, err)

	fakeTargetService := `{"fake-service":{"image":"registry.fake.com/dice/fake-service:fake","image_username":"","image_password":"","cmd":"","ports":[{"port":8060,"protocol":"TCP","l4_protocol":"TCP","expose":true,"default":false}],"envs":{"fakeEnv":"1"},"resources":{"cpu":0.3,"mem":1536,"max_cpu":1,"max_mem":1536,"disk":0,"network":{"mode":"container"}},"deployments":{"replicas":2,"policies":""},"health_check":{"http":{"port":8060,"path":"/fake/health","duration":200},"exec":{}},"traffic_security":{}}}`
	err = json.Unmarshal([]byte(fakeTargetService), &targetService)
	assert.NoError(t, err)

	fakeSG := `{"created_time":1623400858,"last_modified_time":1623908973,"executor":"fake","clusterName":"fake-cluster","force":true,"name":"1f1a1k1e11","namespace":"services","services":[{"name":"fake-service","namespace":"services--1f1a1k1e11","image":"registry.fake.com/dice/fake-service:fake","image_username":"","image_password":"","Ports":[{"port":8060,"protocol":"TCP","l4_protocol":"TCP","expose":true,"default":false}],"proxyPorts":[8060],"vip":"fake-service.services--1f1a1k1e11.svc.cluster.local","shortVIP":"192.168.1.1","proxyIp":"192.168.1.1","scale":2,"resources":{"cpu":0.8,"mem":1537},"health_check":{"http":{"port":8060,"path":"/fake","duration":200}},"traffic_security":{},"status":"Healthy","reason":"","unScheduledReasons":{}}],"serviceDiscoveryKind":"","serviceDiscoveryMode":"DEPEND","projectNamespace":"","status":"Healthy","reason":"","unScheduledReasons":{}}`
	err = json.Unmarshal([]byte(fakeSG), &sg)
	assert.NoError(t, err)

	domainMap["fake-service"] = []string{"http://fake-services-dev-1-app.fake.io"}
	status = "CANCELED"

	fillRuntimeDataWithServiceGroup(&data, targetService, targetJob, &sg, domainMap, status)
	assert.Equal(t, "", data.ModuleErrMsg["fake-service"]["Msg"])
	assert.Equal(t, "", data.ModuleErrMsg["fake-service"]["Reason"])
	assert.Equal(t, 1.6, data.Resources.CPU)
	assert.Equal(t, 3074, data.Resources.Mem)
	assert.Equal(t, 0, data.Resources.Disk)

	assert.Equal(t, 0.8, data.Services["fake-service"].Resources.CPU)
	assert.Equal(t, 1537, data.Services["fake-service"].Resources.Mem)
	assert.Equal(t, 0, data.Services["fake-service"].Resources.Disk)
	assert.Equal(t, "Healthy", data.Services["fake-service"].Status)
	assert.Equal(t, 2, data.Services["fake-service"].Deployments.Replicas)
}

func TestGetRollbackConfig(t *testing.T) {
	var bdl *bundle.Bundle
	monkey.PatchInstanceMethod(reflect.TypeOf(bdl), "GetAllProjects",
		func(*bundle.Bundle) ([]apistructs.ProjectDTO, error) {
			return []apistructs.ProjectDTO{
				{ID: 1, RollbackConfig: map[string]int{"DEV": 3, "TEST": 5, "STAGING": 4, "PROD": 6}},
				{ID: 2, RollbackConfig: map[string]int{"DEV": 4, "TEST": 6, "STAGING": 5, "PROD": 7}},
				{ID: 3, RollbackConfig: map[string]int{"DEV": 5, "TEST": 7, "STAGING": 6, "PROD": 8}},
			}, nil
		},
	)
	defer monkey.UnpatchAll()

	r := New(WithBundle(bdl))
	cfg, err := r.getRollbackConfig()
	assert.NoError(t, err)
	assert.Equal(t, 3, cfg[1]["DEV"])
	assert.Equal(t, 6, cfg[2]["TEST"])
	assert.Equal(t, 6, cfg[3]["STAGING"])
	assert.Equal(t, 8, cfg[3]["PROD"])
}

func Test_getRedeployPipelineYmlName(t *testing.T) {
	type args struct {
		runtime dbclient.Runtime
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		// TODO: Add test cases
		{
			name: "Filled in the space and scene set",
			args: args{
				runtime: dbclient.Runtime{
					ApplicationID: 1,
					Workspace:     "PORD",
					Name:          "master",
				},
			},
			want: "1/PORD/master/pipeline.yml",
		},
		{
			name: "Filled in the space and scene set",
			args: args{
				runtime: dbclient.Runtime{
					ApplicationID: 4,
					Workspace:     "TEST",
					Name:          "master",
				},
			},
			want: "4/TEST/master/pipeline.yml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getRedeployPipelineYmlName(tt.args.runtime); got != tt.want {
				t.Errorf("getRedeployPipelineYmlName() = %v, want %v", got, tt.want)
			}
		})
	}
}

var diceYml = `version: 2.0
services:
  web:
    ports:
      - 8080
      - port: 20880
      - port: 1234
        protocol: "UDP"
      - port: 4321
        protocol: "HTTP"
      - port: 53
        protocol: "DNS"
        l4_protocol: "UDP"
        default: true
    deployments:
      replicas: 1
    resources:
      cpu: 0.1
      mem: 512
    k8s_snippet:
      container:
        name: abc
        stdin: true
        workingDir: aaa
        imagePullPolicy: Always
        securityContext:
          privileged: true
`

func TestGetServicesNames(t *testing.T) {
	name, err := getServicesNames(diceYml)
	if err != nil {
		assert.Error(t, err)
		return
	}
	assert.Equal(t, []string{"web"}, name)
}

func TestConvertRuntimeDeployDto(t *testing.T) {
	app := &apistructs.ApplicationDTO{
		ID:          1,
		Name:        "foo",
		OrgID:       2,
		OrgName:     "erda",
		ProjectID:   3,
		ProjectName: "bar",
	}

	release := &pb.ReleaseGetResponseData{
		Diceyml: diceYml,
	}

	dto := &apistructs.PipelineDTO{ID: 4}

	want := apistructs.RuntimeDeployDTO{
		PipelineID:      4,
		ApplicationID:   1,
		ApplicationName: "foo",
		ProjectID:       3,
		ProjectName:     "bar",
		OrgID:           2,
		OrgName:         "erda",
		ServicesNames:   []string{"web"},
	}
	deployDto, err := convertRuntimeDeployDto(app, release, dto)
	if err != nil {
		assert.Error(t, err)
		return
	}
	assert.Equal(t, want, *deployDto)
}

func Test_setClusterName(t *testing.T) {
	var bdl *bundle.Bundle
	var clusterinfoImpl *clusterinfo.ClusterInfoImpl
	m1 := monkey.PatchInstanceMethod(reflect.TypeOf(clusterinfoImpl), "Info", func(_ *clusterinfo.ClusterInfoImpl, clusterName string) (apistructs.ClusterInfoData, error) {
		if clusterName == "erda-edge" {
			return apistructs.ClusterInfoData{apistructs.JOB_CLUSTER: "erda-center", apistructs.DICE_IS_EDGE: "true"}, nil
		}
		return apistructs.ClusterInfoData{apistructs.DICE_IS_EDGE: "false"}, nil
	})
	defer m1.Unpatch()
	runtimeSvc := New(WithBundle(bdl), WithClusterInfo(clusterinfoImpl))
	rt := &dbclient.Runtime{
		ClusterName: "erda-edge",
	}
	runtimeSvc.setClusterName(rt)
	assert.Equal(t, "erda-center", rt.ClusterName)
}

func Test_convertRuntimeSummaryDTOFromRuntimeModel(t *testing.T) {
	var bdl *bundle.Bundle
	var db *dbclient.DBClient
	runtime := New(WithBundle(bdl), WithDBClient(db))
	a := &apistructs.RuntimeSummaryDTO{}
	r := dbclient.Runtime{
		BaseModel: dbengine.BaseModel{
			ID: 111,
		},
		Name:          "master",
		ApplicationID: 0,
		Workspace:     "",
	}
	d := dbclient.Deployment{
		BaseModel: dbengine.BaseModel{
			ID: 3,
		},
		RuntimeId: 111,
		ReleaseId: "aaaa-bbbbb-cccc",
		Operator:  "erda",
		Status:    "OK",
	}
	_ = runtime.convertRuntimeSummaryDTOFromRuntimeModel(a, r, &d)
	assert.Equal(t, apistructs.DeploymentStatus("OK"), a.DeployStatus)
}

func Test_generateListGroupAppResult(t *testing.T) {
	var bdl *bundle.Bundle
	var db *dbclient.DBClient
	runtime := New(WithBundle(bdl), WithDBClient(db))
	var result = struct {
		sync.RWMutex
		m map[uint64][]*apistructs.RuntimeSummaryDTO
	}{m: make(map[uint64][]*apistructs.RuntimeSummaryDTO)}
	var wg sync.WaitGroup
	r := dbclient.Runtime{
		BaseModel: dbengine.BaseModel{
			ID: 111,
		},
		Name:          "master",
		ApplicationID: 1,
		Workspace:     "",
	}
	d := dbclient.Deployment{
		BaseModel: dbengine.BaseModel{
			ID: 3,
		},
		RuntimeId: 111,
		ReleaseId: "aaaa-bbbbb-cccc",
		Operator:  "erda",
		Status:    "OK",
	}
	wg.Add(1)
	runtime.generateListGroupAppResult(&result, 1, &r, d, &wg)
	assert.Equal(t, apistructs.DeploymentStatus("OK"), result.m[1][0].DeployStatus)
}

func Test_listGroupByApps(t *testing.T) {
	var bdl *bundle.Bundle
	var db *dbclient.DBClient
	m1 := monkey.PatchInstanceMethod(reflect.TypeOf(db), "FindRuntimesInApps", func(_ *dbclient.DBClient, appIDs []uint64, env string) (map[uint64][]*dbclient.Runtime, []uint64, error) {
		a := make(map[uint64][]*dbclient.Runtime)
		a[1] = []*dbclient.Runtime{{
			BaseModel: dbengine.BaseModel{
				ID: 1,
			},
			Name:          "master",
			Workspace:     "DEV",
			ApplicationID: 1,
		}}
		return a, []uint64{1}, nil
	})
	defer m1.Unpatch()

	m2 := monkey.PatchInstanceMethod(reflect.TypeOf(db), "FindLastDeploymentIDsByRutimeIDs", func(_ *dbclient.DBClient, runtimeIDs []uint64) ([]uint64, error) {
		return []uint64{5}, nil
	})
	defer m2.Unpatch()

	m3 := monkey.PatchInstanceMethod(reflect.TypeOf(db), "FindDeploymentsByIDs", func(_ *dbclient.DBClient, ids []uint64) (map[uint64]dbclient.Deployment, error) {
		a := make(map[uint64]dbclient.Deployment)
		a[1] = dbclient.Deployment{
			BaseModel: dbengine.BaseModel{
				ID: 5,
			},
			RuntimeId: 1,
			Status:    "OK",
		}
		return a, nil
	})
	defer m3.Unpatch()
	runtime := New(WithBundle(bdl), WithDBClient(db))
	result, _ := runtime.ListGroupByApps([]uint64{1}, "DEV")
	assert.Equal(t, apistructs.DeploymentStatus("OK"), result[1][0].DeployStatus)
}

func TestPreCheck(t *testing.T) {
	r := New()
	a := addon.New()
	type args struct {
		diceYaml  string
		workspace string
	}

	defer monkey.UnpatchAll()

	monkey.PatchInstanceMethod(reflect.TypeOf(a), "GetAddonExtention", func(a *addon.Addon, params *apistructs.AddonHandlerCreateItem) (*apistructs.AddonExtension, *diceyml.Object, error) {
		addonName := params.AddonName
		if strings.Contains(addonName, "nonExistAddon") {
			return nil, nil, errors.New("not found")
		}

		if addonName == apistructs.AddonCustomCategory {
			return &apistructs.AddonExtension{
				SubCategory: apistructs.BasicAddon,
				Category:    apistructs.AddonCustomCategory,
			}, nil, nil
		}

		return &apistructs.AddonExtension{
			SubCategory: apistructs.BasicAddon,
		}, nil, nil
	})

	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "basic addon deploy to no-prod environment",
			args: args{
				diceYaml: `
version: 2
addons:
  rds:
    plan: custom:basic
    options:
      version: 1.0.0
  addon-1:
    plan: redis:basic
    options:
      version: 3.2.12
`,
				workspace: apistructs.WORKSPACE_TEST,
			},
			wantErr: false,
		},
		{
			name: "basic addon deploy to prod environment",
			args: args{
				diceYaml: `
version: 2
addons:
  rds:
    plan: custom:basic
    options:
      version: 1.0.0
  addon-1:
    plan: redis:basic
    options:
      version: 3.2.12
`,
				workspace: apistructs.WORKSPACE_PROD,
			},
			// TODO: precheck should return error
			wantErr: false,
		},
		{
			name: "professional addon deploy to prod environment",
			args: args{
				diceYaml: `
version: 2
addons:
  rds:
    plan: custom
    options:
      version: 1.0.0
  addon-1:
    plan: redis:professional
    options:
      version: 3.2.12
`,
				workspace: apistructs.WORKSPACE_PROD,
			},
			wantErr: false,
		},
		{
			name: "multi addons deploy to prod environment, had non-exist addon and plan error addon",
			args: args{
				diceYaml:  generateMultiAddons(t, 200),
				workspace: apistructs.WORKSPACE_PROD,
			},
			// TODO: precheck should return error
			wantErr: false,
		},
		{
			name: "non addons deploy to prod environment",
			args: args{
				diceYaml:  generateMultiAddons(t, 0),
				workspace: apistructs.WORKSPACE_PROD,
			},
			wantErr: false,
		},
		{
			name: "illegal addon plan format",
			args: args{
				diceYaml: `
version: 2
addons:
  rds:
    plan: custom:basic:err
    options:
      version: 1.0.0
`,
				workspace: apistructs.WORKSPACE_PROD,
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dice, err := diceyml.New([]byte(test.args.diceYaml), false)
			if err != nil {
				assert.NoError(t, err)
			}

			if err := r.PreCheck(dice, test.args.workspace); (err != nil) != test.wantErr {
				t.Errorf("PreCheck error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func generateMultiAddons(t *testing.T, randCount int) string {
	var addonsCount int
	if randCount != 0 {
		addonsCount = rand.Intn(randCount)
	}

	addons := make(diceyml.AddOns)
	for i := 0; i < addonsCount; i++ {
		name := fmt.Sprintf("existAddon%d", i)
		plan := fmt.Sprintf("existAddon%d:%s", i, apistructs.AddonUltimate)
		addons[name] = &diceyml.AddOn{
			Plan: plan,
			Options: map[string]string{
				"version": "1.0.0",
			},
		}
	}

	if addonsCount != 0 {
		addons["nonExistAddon1"] = &diceyml.AddOn{
			Plan: fmt.Sprintf("nonExistAddon1:%s", apistructs.AddonBasic),
			Options: map[string]string{
				"version": "1.0.0",
			},
		}
	}

	diceObj := diceyml.Object{
		Version: "2.0",
		AddOns:  addons,
	}

	diceYaml, err := yaml.Marshal(diceObj)
	assert.NoError(t, err)

	return string(diceYaml)
}

type orgMock struct {
	mock.OrgMock
}

func (m orgMock) GetOrg(ctx context.Context, request *orgpb.GetOrgRequest) (*orgpb.GetOrgResponse, error) {
	if request.IdOrName == "" {
		return nil, fmt.Errorf("the IdOrName is empty")
	}
	return &orgpb.GetOrgResponse{Data: &orgpb.Org{}}, nil
}

func TestRuntime_GetOrg(t *testing.T) {
	type fields struct {
		org org.ClientInterface
	}
	type args struct {
		orgID uint64
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    *orgpb.Org
		wantErr bool
	}{
		{
			name: "test with error",
			fields: fields{
				org: orgMock{},
			},
			args:    args{orgID: 0},
			want:    nil,
			wantErr: true,
		},
		{
			name: "test with no error",
			fields: fields{
				org: orgMock{},
			},
			args:    args{orgID: 1},
			want:    &orgpb.Org{},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runtime{
				org: tt.fields.org,
			}
			got, err := r.GetOrg(tt.args.orgID)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetOrg() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetOrg() got = %v, want %v", got, tt.want)
			}
		})
	}
}
