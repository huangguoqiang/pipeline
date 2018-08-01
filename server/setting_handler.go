package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/rancher/go-rancher/api"
	v1client "github.com/rancher/go-rancher/client"
	"github.com/rancher/pipeline/model"
	"github.com/rancher/pipeline/server/service"
)

//Get pipelineSetting Handler
func (s *Server) GetPipelineSetting(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	a, err := service.GetPipelineSetting()
	if err != nil {
		return err
	}
	model.ToPipelineSettingResource(apiContext, a)

	return apiContext.WriteResource(a)
}

func (s *Server) UpdatePipelineSetting(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	requestBytes, err := ioutil.ReadAll(req.Body)
	setting := &model.PipelineSetting{}

	if err := json.Unmarshal(requestBytes, setting); err != nil {
		return err
	}

	err = service.CreateOrUpdatePipelineSetting(setting)
	if err != nil {
		return err
	}
	model.ToPipelineSettingResource(apiContext, setting)
	apiContext.Write(setting)
	return nil

}

func (s *Server) ListSCMSetting(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	settings := service.ListSCMSetting()
	result := []interface{}{}
	for _, setting := range settings {
		model.ToSCMSettingResource(apiContext, setting)
		result = append(result, setting)
	}
	apiContext.Write(&v1client.GenericCollection{
		Data: result,
	})
	return nil
}

func (s *Server) GetSCMSetting(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	id := mux.Vars(req)["id"]
	a, err := service.GetSCMSetting(id)
	if err != nil {
		return err
	}
	if !a.IsAuth {
		return fmt.Errorf("source code manager setting for '%s' is not enabled", id)
	}
	model.ToSCMSettingResource(apiContext, a)
	return apiContext.WriteResource(a)
}

func (s *Server) UpdateSCMSetting(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	requestBytes, err := ioutil.ReadAll(req.Body)
	setting := &model.SCMSetting{}

	if err := json.Unmarshal(requestBytes, setting); err != nil {
		return err
	}

	err = service.CreateOrUpdateSCMSetting(setting)
	if err != nil {
		return err
	}
	broadcastResourceChange(*setting)
	if setting.IsAuth == false {
		delAccounts, err := service.CleanAccounts(setting.ScmType)
		if err != nil {
			return err
		}
		for _, account := range delAccounts {
			account.Status = "removed"
			broadcastResourceChange(*account)
		}
	}
	model.ToSCMSettingResource(apiContext, setting)
	apiContext.Write(setting)
	return nil

}

func (s *Server) RemoveSCMSetting(rw http.ResponseWriter, req *http.Request) error {
	id := mux.Vars(req)["id"]

	delAccounts, err := service.CleanAccounts(id)
	if err != nil {
		return err
	}
	for _, account := range delAccounts {
		account.Status = "removed"
		broadcastResourceChange(*account)
	}

	setting, err := service.RemoveSCMSetting(id)
	if err != nil {
		return err
	}
	setting.Status = "removed"
	broadcastResourceChange(*setting)
	return nil
}
