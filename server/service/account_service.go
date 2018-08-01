package service

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Sirupsen/logrus"
	"github.com/rancher/go-rancher/v2"
	"github.com/rancher/pipeline/model"
	"github.com/rancher/pipeline/util"
)

const GIT_ACCOUNT_TYPE = "gitaccount"
const REPO_CACHE_TYPE = "repocache"

func RefreshRepos(accountId string) ([]*model.GitRepository, error) {

	account, err := GetAccount(accountId)
	if err != nil {
		return nil, err
	}
	manager, err := GetSCManager(account.AccountType)
	if err != nil {
		return nil, err
	}
	repos, err := manager.GetRepos(account)
	if err != nil {
		return nil, err
	}
	return repos, CreateOrUpdateCacheRepoList(accountId, repos)
}

func GetAccount(id string) (*model.GitAccount, error) {
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return nil, err
	}
	filters := make(map[string]interface{})
	filters["kind"] = GIT_ACCOUNT_TYPE
	filters["key"] = id
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		return nil, fmt.Errorf("Error %v filtering genericObjects by key", err)
	}
	if len(goCollection.Data) == 0 {
		return nil, fmt.Errorf("cannot find account with id '%s'", id)
	}
	data := goCollection.Data[0]
	account := &model.GitAccount{}
	if err = json.Unmarshal([]byte(data.ResourceData["data"].(string)), &account); err != nil {
		return nil, err
	}
	return account, nil
}

//listAccounts gets scm accounts accessible by the user
func ListAccounts(uid string) ([]*model.GitAccount, error) {
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return nil, err
	}
	filters := make(map[string]interface{})
	filters["kind"] = GIT_ACCOUNT_TYPE
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		return nil, fmt.Errorf("Error %v filtering genericObjects by key", err)
	}
	var accounts []*model.GitAccount
	for _, gobj := range goCollection.Data {
		b := []byte(gobj.ResourceData["data"].(string))
		a := &model.GitAccount{}
		json.Unmarshal(b, a)
		if uid == a.RancherUserID || !a.Private {
			accounts = append(accounts, a)
		}
	}

	return accounts, nil
}

func ShareAccount(id string) (*model.GitAccount, error) {

	r, err := GetAccount(id)
	if err != nil {
		logrus.Errorf("fail getting account with id:%v", id)
		return nil, err
	}
	if r.Private {
		r.Private = false
		if err := UpdateAccount(r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func UnshareAccount(id string) (*model.GitAccount, error) {
	r, err := GetAccount(id)
	if err != nil {
		logrus.Errorf("fail getting account with id:%v", id)
		return nil, err
	}
	if !r.Private {
		r.Private = true
		if err := UpdateAccount(r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func UpdateAccount(account *model.GitAccount) error {
	b, err := json.Marshal(account)
	if err != nil {
		return err
	}
	resourceData := map[string]interface{}{
		"data": string(b),
	}
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return err
	}

	filters := make(map[string]interface{})
	filters["key"] = account.Id
	filters["kind"] = GIT_ACCOUNT_TYPE
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		logrus.Errorf("Error querying account:%v", err)
		return err
	}
	if len(goCollection.Data) == 0 {
		return fmt.Errorf("account '%s' not found", account.Id)
	}
	existing := goCollection.Data[0]
	_, err = apiClient.GenericObject.Update(&existing, &client.GenericObject{
		Name:         account.Id,
		Key:          account.Id,
		ResourceData: resourceData,
		Kind:         GIT_ACCOUNT_TYPE,
	})
	return err
}

func RemoveAccount(id string) (*model.GitAccount, error) {
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return nil, err
	}

	filters := make(map[string]interface{})
	filters["key"] = id
	filters["kind"] = GIT_ACCOUNT_TYPE
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		logrus.Errorf("Error querying account:%v", err)
		return nil, err
	}
	if len(goCollection.Data) == 0 {
		return nil, fmt.Errorf("account '%s' not found", id)
	}
	existing := goCollection.Data[0]

	account := &model.GitAccount{}
	if err = json.Unmarshal([]byte(existing.ResourceData["data"].(string)), account); err != nil {
		return nil, err
	}
	if err = apiClient.GenericObject.Delete(&existing); err != nil {
		return nil, err
	}

	return account, nil
}

func CleanAccounts(scmType string) ([]*model.GitAccount, error) {

	apiClient, err := util.GetRancherClient()
	if err != nil {
		return nil, err
	}
	geObjList, err := PaginateGenericObjects(GIT_ACCOUNT_TYPE)
	if err != nil {
		logrus.Errorf("fail to list acciybt,err:%v", err)
		return nil, err
	}
	delAccounts := []*model.GitAccount{}
	for _, gobj := range geObjList {
		account := &model.GitAccount{}
		if err := json.Unmarshal([]byte(gobj.ResourceData["data"].(string)), account); err != nil {
			logrus.Errorf("parse data got error:%v", err)
			continue
		}
		if account.AccountType == scmType {
			delAccounts = append(delAccounts, account)
			apiClient.GenericObject.Delete(&gobj)
		}
	}
	return delAccounts, nil
}

func CreateAccount(account *model.GitAccount) error {
	b, err := json.Marshal(account)
	if err != nil {
		return err
	}
	resourceData := map[string]interface{}{
		"data": string(b),
	}
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return err
	}
	_, err = apiClient.GenericObject.Create(&client.GenericObject{
		Name:         account.Id,
		Key:          account.Id,
		ResourceData: resourceData,
		Kind:         GIT_ACCOUNT_TYPE,
	})
	return err
}

func GetCacheRepoList(accountId string) ([]*model.GitRepository, error) {
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return nil, err
	}
	filters := make(map[string]interface{})
	filters["kind"] = REPO_CACHE_TYPE
	filters["key"] = accountId
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		return nil, fmt.Errorf("Error %v filtering genericObjects by key", err)
	}
	if len(goCollection.Data) == 0 {
		//no cache,refresh
		repos, err := RefreshRepos(accountId)
		if err != nil {
			return nil, err
		}
		return repos, nil
	}
	repos := []*model.GitRepository{}
	data := goCollection.Data[0]
	if err = json.Unmarshal([]byte(data.ResourceData["data"].(string)), &repos); err != nil {
		return nil, err
	}
	return repos, nil
}

func CreateOrUpdateCacheRepoList(accountId string, repos []*model.GitRepository) error {

	logrus.Debugf("refreshing repos")
	b, err := json.Marshal(repos)
	if err != nil {
		return err
	}
	resourceData := map[string]interface{}{
		"data": string(b),
	}
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return err
	}

	filters := make(map[string]interface{})
	filters["key"] = accountId
	filters["kind"] = REPO_CACHE_TYPE
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		logrus.Errorf("Error querying account:%v", err)
		return err
	}
	if len(goCollection.Data) == 0 {
		//not exist,create a repocache object
		if _, err := apiClient.GenericObject.Create(&client.GenericObject{
			Key:          accountId,
			ResourceData: resourceData,
			Kind:         REPO_CACHE_TYPE,
		}); err != nil {
			return fmt.Errorf("Save repo cache got error: %v", err)
		}

		logrus.Debugf("done refresh repos")
		return nil
	}
	existing := goCollection.Data[0]
	_, err = apiClient.GenericObject.Update(&existing, &client.GenericObject{
		Key:          accountId,
		ResourceData: resourceData,
		Kind:         REPO_CACHE_TYPE,
	})
	return err
}

func CreateCredential(cred *model.Credential) error {
	b, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	resourceData := map[string]interface{}{
		"data": string(b),
	}
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return err
	}
	_, err = apiClient.GenericObject.Create(&client.GenericObject{
		Name:         cred.Id,
		Key:          cred.Id,
		ResourceData: resourceData,
		Kind:         "pipelineCred",
	})
	return err
}

func UpdateCredential(cred *model.Credential) error {
	b, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	resourceData := map[string]interface{}{
		"data": string(b),
	}
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return err
	}

	filters := make(map[string]interface{})
	filters["key"] = cred.Id
	filters["kind"] = "pipelineCred"
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		logrus.Errorf("Error querying account:%v", err)
		return err
	}
	if len(goCollection.Data) == 0 {
		return fmt.Errorf("credential '%s' not found", cred.Id)
	}
	existing := goCollection.Data[0]
	_, err = apiClient.GenericObject.Update(&existing, &client.GenericObject{
		Name:         cred.Id,
		Key:          cred.Id,
		ResourceData: resourceData,
		Kind:         "pipelineCred",
	})
	return err
}

func GetEnvKey(clientId string) (string, error) {
	id := "envKey:" + clientId
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return "", err
	}
	filters := make(map[string]interface{})
	filters["kind"] = "pipelineCred"
	filters["key"] = id
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		return "", fmt.Errorf("Error %v filtering genericObjects by key", err)
	}
	if len(goCollection.Data) == 0 {
		return "", nil
	}
	a := &model.Credential{}
	data := goCollection.Data[0]
	if err = json.Unmarshal([]byte(data.ResourceData["data"].(string)), a); err != nil {
		return "", err
	}
	return a.SecretValue, nil
}

func CreateOrUpdateEnvKey(clientId string, token string) error {
	id := "envKey:" + clientId
	apiClient, err := util.GetRancherClient()
	if err != nil {
		return err
	}
	filters := make(map[string]interface{})
	filters["kind"] = "pipelineCred"
	filters["key"] = id
	goCollection, err := apiClient.GenericObject.List(&client.ListOpts{
		Filters: filters,
	})
	if err != nil {
		return fmt.Errorf("Error %v filtering genericObjects by key", err)
	}
	cred := &model.Credential{
		CredType:    "envKey",
		PublicValue: clientId,
		SecretValue: token,
	}
	cred.Id = id
	if len(goCollection.Data) == 0 {
		//not exist, create new
		if err := CreateCredential(cred); err != nil {
			return err
		}
	} else {
		//update
		if err := UpdateCredential(cred); err != nil {
			return err
		}
	}
	return nil
}

func ValidAccountAccess(req *http.Request, accountId string) bool {
	return true
}

func ValidAccountAccessById(uid string, accountId string) bool {
	return true
}

func GetUserToken(gitUser string) (string, error) {
	account, err := GetAccount(gitUser)
	if err != nil {
		return "", err
	}
	return account.AccessToken, nil
}
