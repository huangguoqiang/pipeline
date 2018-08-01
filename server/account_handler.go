package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/rancher/go-rancher/api"
	v1client "github.com/rancher/go-rancher/client"
	"github.com/rancher/pipeline/model"
	"github.com/rancher/pipeline/server/service"
	"github.com/rancher/pipeline/util"
)

func (s *Server) ListAccounts(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	uid, err := util.GetCurrentUser(req.Cookies())
	if err != nil || uid == "" {
		if err != nil {
			logrus.Errorf("get user error:%v", err)
		}
		logrus.Warning("fail to get current user, trying in envrionment scope")
	}
	accounts, err := service.ListAccounts(uid)
	if err != nil {
		return err
	}
	result := []interface{}{}
	for _, account := range accounts {
		result = append(result, model.ToAccountResource(apiContext, account))
	}
	apiContext.Write(&v1client.GenericCollection{
		Data: result,
	})
	return nil
}

func (s *Server) GetAccount(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)

	id := mux.Vars(req)["id"]
	if !service.ValidAccountAccess(req, id) {
		return fmt.Errorf("cannot access account '%s'", id)
	}
	r, err := service.GetAccount(id)
	if err != nil {
		return err
	}
	return apiContext.WriteResource(model.ToAccountResource(apiContext, r))
}

func (s *Server) RemoveAccount(rw http.ResponseWriter, req *http.Request) error {
	id := mux.Vars(req)["id"]
	if !service.ValidAccountAccess(req, id) {
		return fmt.Errorf("cannot access account '%s'", id)
	}
	a, err := service.GetAccount(id)
	if err != nil {
		return err
	}
	account, err := service.RemoveAccount(id)
	if err != nil {
		return err
	}
	if err := s.Provider.OnDeleteAccount(account); err != nil {
		return err
	}
	a.Status = "removed"
	broadcastResourceChange(*a)
	return nil
}

func (s *Server) ShareAccount(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	id := mux.Vars(req)["id"]
	if !service.ValidAccountAccess(req, id) {
		return fmt.Errorf("cannot access account '%s'", id)
	}
	a, err := service.ShareAccount(id)
	if err != nil {
		return err
	}

	return apiContext.WriteResource(model.ToAccountResource(apiContext, a))
}

func (s *Server) UnshareAccount(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	id := mux.Vars(req)["id"]
	if !service.ValidAccountAccess(req, id) {
		return fmt.Errorf("cannot access account '%s'", id)
	}
	a, err := service.UnshareAccount(id)
	if err != nil {
		return err
	}
	return apiContext.WriteResource(model.ToAccountResource(apiContext, a))
}

func (s *Server) RefreshRepos(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	id := mux.Vars(req)["id"]
	repos, err := service.RefreshRepos(id)
	if err != nil {
		return err
	}
	result := []interface{}{}
	for _, repo := range repos {
		result = append(result, model.ToRepositoryResource(apiContext, repo))
	}
	apiContext.Write(&v1client.GenericCollection{
		Data: result,
	})
	return nil
}

func (s *Server) GetCacheRepos(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	id := mux.Vars(req)["id"]
	if !service.ValidAccountAccess(req, id) {
		return fmt.Errorf("cannot access account '%s'", id)
	}
	repos, err := service.GetCacheRepoList(id)
	if err != nil {
		return err
	}
	result := []interface{}{}
	for _, repo := range repos {
		result = append(result, model.ToRepositoryResource(apiContext, repo))
	}
	apiContext.Write(&v1client.GenericCollection{
		Data: result,
	})
	return nil
}

func (s *Server) Oauth(rw http.ResponseWriter, req *http.Request) error {
	apiContext := api.GetApiContext(req)
	requestBody := make(map[string]interface{})
	requestBytes, err := ioutil.ReadAll(req.Body)
	if err := json.Unmarshal(requestBytes, &requestBody); err != nil {
		return err
	}
	var code, scmType, clientID, clientSecret, redirectURL, scheme, hostName string
	if requestBody["code"] != nil {
		code = requestBody["code"].(string)
	}
	if requestBody["clientID"] != nil {
		clientID = requestBody["clientID"].(string)
	}
	if requestBody["clientSecret"] != nil {
		clientSecret = requestBody["clientSecret"].(string)
	}
	if requestBody["redirectURL"] != nil {
		redirectURL = requestBody["redirectURL"].(string)
	}
	if requestBody["scheme"] != nil {
		scheme = requestBody["scheme"].(string)
	}
	if requestBody["hostName"] != nil {
		hostName = requestBody["hostName"].(string)
	}
	if requestBody["scmType"] != nil {
		scmType = requestBody["scmType"].(string)
	}

	var account *model.GitAccount
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		setting, err := service.GetSCMSetting(scmType)
		if err != nil {
			return err
		}
		if !setting.IsAuth {
			return fmt.Errorf("auth not set")
		}
		clientID = setting.ClientID
		clientSecret = setting.ClientSecret
		redirectURL = setting.RedirectURL

		SCManager, err := service.GetSCManager(scmType)
		if err != nil {
			return err
		}

		account, err = SCManager.OAuth(redirectURL, clientID, clientSecret, code)
		if err != nil {
			return err
		}
	} else {
		setting := &model.SCMSetting{}
		setting.IsAuth = true
		setting.Id = scmType
		setting.ClientID = clientID
		setting.ClientSecret = clientSecret
		setting.RedirectURL = redirectURL
		setting.Scheme = scheme
		setting.HostName = hostName
		setting.ScmType = scmType
		SCManager, err := service.GetSCManagerFromSetting(setting)
		if err != nil {
			return err
		}
		account, err = SCManager.OAuth(redirectURL, clientID, clientSecret, code)
		if err != nil {
			return err
		}
		//init scmSetting on success
		if err := service.CreateOrUpdateSCMSetting(setting); err != nil {
			return err
		}
	}
	uid, err := util.GetCurrentUser(req.Cookies())
	if err == nil && uid != "" {
		account.RancherUserID = uid
	}
	existing, err := service.GetAccount(account.Id)
	if err == nil && existing != nil {
		//git account exists
		return fmt.Errorf("%s account '%s' is authed, to add another account using oauth, you need to log out on %s first", account.AccountType, account.Login, account.AccountType)
	}

	//new account added
	if err := service.CreateAccount(account); err != nil {
		return err
	}

	s.Provider.OnCreateAccount(account)

	broadcastResourceChange(*account)
	go service.RefreshRepos(account.Id)
	setting, err := service.GetSCMSetting(scmType)
	if err != nil {
		return err
	}
	broadcastResourceChange(*setting)
	model.ToSCMSettingResource(apiContext, setting)
	if err = apiContext.WriteResource(setting); err != nil {
		return err
	}
	return nil
}
