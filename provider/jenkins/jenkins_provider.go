package jenkins

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bytes"
	"path"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	"github.com/rancher/pipeline/model"
	"github.com/rancher/pipeline/server/service"
	"github.com/rancher/pipeline/util"
	"github.com/sluu99/uuid"
)

type JenkinsProvider struct {
}

func (j JenkinsProvider) RunPipeline(p *model.Pipeline, triggerType string) (*model.Activity, error) {

	activity, err := ToActivity(p)
	if err != nil {
		return nil, err
	}
	activity.TriggerType = triggerType
	initActivityEnvvars(activity)

	if len(p.Stages) == 0 {
		return nil, errors.New("no stage in pipeline definition to run!")
	}
	for i := 0; i < len(p.Stages); i++ {
		logrus.Debugf("creating stage:%v", p.Stages[i])
		if err := j.CreateStage(activity, i); err != nil {
			logrus.Error(errors.Wrapf(err, "stage <%s> fail", p.Stages[i].Name))
			return nil, err
		}
	}
	logrus.Debugf("running stage:%v", p.Stages[0])
	if err = j.RunStage(activity, 0); err != nil {
		return nil, err
	}

	logrus.Debugf("creating activity:%v", activity)

	if err = service.CreateActivity(activity); err != nil {
		return nil, err
	}

	return activity, nil
}

//RerunActivity runs an existing activity
func (j JenkinsProvider) RerunActivity(a *model.Activity) error {

	jobName := getJobName(a, 0, 0)
	_, err := GetJobInfo(jobName)
	if err != nil {
		//job records are missing in jenkins, regenerate them
		for i := 0; i < len(a.Pipeline.Stages); i++ {
			if err := j.CreateStage(a, i); err != nil {
				logrus.Error(errors.Wrapf(err, "recreate stage <%s> fail", a.Pipeline.Stages[i].Name))
				return err
			}
		}
	} else {
		//clean previous build
		if err := DeleteFormerBuild(a); err != nil {
			return err
		}
	}
	//find an available node to run
	nodeName, err := getNodeNameToRun()
	if err != nil {
		return err
	}
	a.NodeName = nodeName
	err = j.UpdateJobConf(a)
	if err != nil {
		logrus.Errorf("fail to update job config before rerun: %v", err)
	}

	logrus.Infof("rerunpipeline,get nodeName:%v", nodeName)
	a.RunSequence = a.Pipeline.RunCount + 1
	a.StartTS = time.Now().UnixNano() / int64(time.Millisecond)
	initActivityEnvvars(a)
	err = j.RunStage(a, 0)
	return err
}

func (j JenkinsProvider) StopActivity(a *model.Activity) error {
	logrus.Debugf("stopping activity, current status: %s", a.Status)
	a.Status = model.ActivityAbort
	now := time.Now().UnixNano() / int64(time.Millisecond)
	a.StopTS = now
	for stageOrdinal, stage := range a.ActivityStages {
		if stage.Status == model.ActivityStageSuccess || stage.Status == model.ActivityStageSkip {
			continue
		} else {
			for stepOrdinal := 0; stepOrdinal < len(stage.ActivitySteps); stepOrdinal++ {
				if err := j.StopStep(a, stageOrdinal, stepOrdinal); err != nil {
					logrus.Errorf("stop step got: %v", err)
					continue
				}
			}
			logrus.Debugf("aborting stage, current status: %s", stage.Status)
			stage.Status = model.ActivityStageAbort
			stage.Duration = now - stage.StartTS
			break
		}

	}
	return nil
}

func (j JenkinsProvider) StopStep(a *model.Activity, stageOrdinal int, stepOrdinal int) error {
	jobname := getJobName(a, stageOrdinal, stepOrdinal)
	info, err := GetJobInfo(jobname)
	if err != nil {
		return err
	}
	step := a.ActivityStages[stageOrdinal].ActivitySteps[stepOrdinal]
	logrus.Debugf("aborting step, current status: %s", step.Status)
	if info.InQueue {
		//delete in queue
		queueItem, ok := info.QueueItem.(map[string]interface{})
		if !ok {
			return fmt.Errorf("type assertion fail for queueitem")
		}
		queueId, ok := queueItem["id"].(float64)
		if !ok {
			return fmt.Errorf("type assertion fail for queueId")
		}
		if err := CancelQueueItem(int(queueId)); err != nil {
			return fmt.Errorf("cancel queueitem error:%v", err)
		}
	} else {
		buildInfo, err := GetBuildInfo(jobname)
		if err != nil {
			return err
		}
		if buildInfo.Building {
			if err := StopJob(jobname); err != nil {
				return err
			}
			step.Status = model.ActivityStepAbort
			step.Duration = time.Now().UnixNano()/int64(time.Millisecond) - step.StartTS
		}
	}
	return nil
}

//CreateStage init jenkins projects settings of the stage, each step forms a jenkins job.
func (j JenkinsProvider) CreateStage(activity *model.Activity, ordinal int) error {
	logrus.Info("create jenkins job from stage")
	stage := activity.ActivityStages[ordinal]
	for i, _ := range stage.ActivitySteps {
		conf := j.generateStepJenkinsProject(activity, ordinal, i)
		jobName := getJobName(activity, ordinal, i)
		bconf, _ := xml.MarshalIndent(conf, "  ", "    ")
		if err := CreateJob(jobName, bconf); err != nil {
			return err
		}
	}
	return nil
}

//getNodeNameToRun gets a random node name to run
func getNodeNameToRun() (string, error) {
	nodes, err := GetActiveNodesName()
	if err != nil {
		return "", errors.Wrapf(err, "fail to find an active node to work")
	}
	if len(nodes) == 0 {
		return "", errors.New("no active worker node available, please add at least one slave node or check if it is ready")
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	index := r.Intn(len(nodes))
	logrus.Debugf("pick %s to work", nodes[index])
	return nodes[index], nil
}

//DeleteFormerBuild delete last build info of a completed activity
func DeleteFormerBuild(activity *model.Activity) error {
	for stageOrdinal, stage := range activity.ActivityStages {
		for stepOrdinal, step := range stage.ActivitySteps {
			jobName := getJobName(activity, stageOrdinal, stepOrdinal)
			if step.Status == model.ActivityStepSuccess || step.Status == model.ActivityStepFail {
				logrus.Infof("deleting:%v", jobName)
				if err := DeleteBuild(jobName); err != nil {
					return err
				}
			}
		}
	}
	return nil

}

//UpdateJobConf update jenkins job config
//use commit id in activity and try with a valid node.
func (j JenkinsProvider) UpdateJobConf(activity *model.Activity) error {
	for stageNum := 0; stageNum < len(activity.ActivityStages); stageNum++ {
		for stepNum := 0; stepNum < len(activity.ActivityStages[stageNum].ActivitySteps); stepNum++ {
			conf := j.generateStepJenkinsProject(activity, stageNum, stepNum)
			if stageNum == 0 && stepNum == 0 && activity.CommitInfo != "" && activity.CommitInfo != "null" {
				conf.Scm.GitBranch = activity.CommitInfo
			}
			jobName := getJobName(activity, stageNum, stepNum)
			bconf, _ := xml.MarshalIndent(conf, "  ", "    ")
			logrus.Debugf("updating jenkins job:%s", jobName)
			if err := UpdateJob(jobName, bconf); err != nil {
				logrus.Errorf("updatejob error:%v", err)
				return err
			}
		}
	}
	return nil
}

func EvaluateConditions(activity *model.Activity, condition *model.PipelineConditions) (bool, error) {
	if condition == nil || (len(condition.All) == 0 && len(condition.Any) == 0) {
		return false, fmt.Errorf("Nil condition")
	}
	if len(condition.All) > 0 {
		for _, c := range condition.All {
			resCond, err := EvaluateCondition(activity, c)
			if err != nil {
				return false, err
			}
			if !resCond {
				return false, nil
			}
		}
		return true, nil
	}

	for _, c := range condition.Any {
		resCond, err := EvaluateCondition(activity, c)
		if err != nil {
			return false, err
		}
		if resCond {
			return true, nil
		}
	}
	return false, nil
}

//valid format:     xxx=xxx; xxx!=xxx
func EvaluateCondition(activity *model.Activity, condition string) (bool, error) {
	m := util.GetParams(`(?P<Key>.*?)!=(?P<Value>.*)`, condition)
	if m["Key"] != "" && m["Value"] != "" {
		key := SubstituteVar(activity, m["Key"])
		val := SubstituteVar(activity, m["Value"])
		envVal := activity.EnvVars[key]
		if envVal != val {
			return true, nil
		}
		return false, nil
	}

	m = util.GetParams(`(?P<Key>.*?)=(?P<Value>.*)`, condition)
	if m["Key"] != "" && m["Value"] != "" {
		key := SubstituteVar(activity, m["Key"])
		val := SubstituteVar(activity, m["Value"])
		envVal := activity.EnvVars[key]
		if envVal == val {
			return true, nil
		}
		return false, nil
	}
	return false, fmt.Errorf("cannot parse condition:%s", condition)
}

func (j JenkinsProvider) RunStage(activity *model.Activity, ordinal int) error {
	if len(activity.ActivityStages) <= ordinal {
		return fmt.Errorf("error run stage,stage index out of range")
	}
	stage := activity.Pipeline.Stages[ordinal]
	logrus.Infof("run stage:%s", stage.Name)
	logrus.Debugf("paras:%v,%v,%v,%v", activity.Pipeline, activity, len(activity.Pipeline.Stages), ordinal)
	condFlag := true
	curTime := time.Now().UnixNano() / int64(time.Millisecond)
	var err error
	if service.HasStageCondition(stage) {
		condFlag, err = EvaluateConditions(activity, stage.Conditions)
		if err != nil {
			logrus.Errorf("Evaluate condition '%v' got error:%v", stage.Conditions, err)
			return err
		}
	}
	if !condFlag {
		activity.ActivityStages[ordinal].Status = model.ActivityStageSkip
		if ordinal == len(activity.ActivityStages)-1 {
			//skip last stage and success activity
			activity.Status = model.ActivitySuccess
			activity.StopTS = curTime
			j.OnActivityCompelte(activity)
		} else {
			//skip the stage then run next one.
			err = j.RunStage(activity, ordinal+1)
		}
		return err
	}

	activity.ActivityStages[ordinal].StartTS = curTime
	//Trigger all step jobs in the stage.
	if stage.Parallel {
		for i := 0; i < len(stage.Steps); i++ {
			if err := j.RunStep(activity, ordinal, i); err != nil {
				logrus.Errorf("run step error:%v", err)
				return err
			}
		}
	} else {
		//Trigger first to run sequentially
		if err := j.RunStep(activity, ordinal, 0); err != nil {
			logrus.Errorf("run step error:%v", err)
			return err
		}
	}

	return nil
}

func (j JenkinsProvider) RunStep(activity *model.Activity, stageOrdinal int, stepOrdinal int) error {
	if len(activity.ActivityStages) <= stageOrdinal ||
		len(activity.ActivityStages[stageOrdinal].ActivitySteps) <= stepOrdinal ||
		stageOrdinal < 0 || stepOrdinal < 0 {
		return fmt.Errorf("error run stage,stage index out of range")
	}
	stage := activity.Pipeline.Stages[stageOrdinal]
	step := stage.Steps[stepOrdinal]
	condFlag := true
	var err error
	if service.HasStepCondition(step) {
		condFlag, err = EvaluateConditions(activity, step.Conditions)
		if err != nil {
			logrus.Errorf("Evaluate condition '%v' got error:%v", step.Conditions, err)
			return err
		}
	}
	if !condFlag {
		activity.ActivityStages[stageOrdinal].ActivitySteps[stepOrdinal].Status = model.ActivityStepSkip
		actiStage := activity.ActivityStages[stageOrdinal]
		curTime := time.Now().UnixNano() / int64(time.Millisecond)
		if service.IsStageSuccess(actiStage) {
			//if skipped and stage success
			actiStage.Status = model.ActivityStageSuccess
			actiStage.Duration = curTime - actiStage.StartTS
			if stageOrdinal == len(activity.ActivityStages)-1 {
				//last stage success and success activity
				activity.Status = model.ActivitySuccess
				activity.StopTS = curTime
				j.OnActivityCompelte(activity)
			} else {
				//success the stage then run next one.
				err = j.RunStage(activity, stageOrdinal+1)
			}
		} else if !stage.Parallel {
			//sequential, skipped current step then run next step
			err = j.RunStep(activity, stageOrdinal, stepOrdinal+1)
		}
		return err
	}
	logrus.Debugf("Run step:%s,%d,%d", activity.Pipeline.Name, stageOrdinal, stepOrdinal)
	jobName := getJobName(activity, stageOrdinal, stepOrdinal)
	if _, err := BuildJob(jobName, map[string]string{}); err != nil {
		logrus.Errorf("run %s error:%v", jobName, err)
		return err
	}

	return nil
}

func (j JenkinsProvider) generateStepJenkinsProject(activity *model.Activity, stageOrdinal int, stepOrdinal int) *JenkinsProject {
	logrus.Info("generating jenkins project config")
	activityId := activity.Id
	workspaceName := path.Join("${JENKINS_HOME}", "workspace", activityId)
	stage := activity.Pipeline.Stages[stageOrdinal]
	step := stage.Steps[stepOrdinal]

	step.Services = service.GetServices(activity, stageOrdinal, stepOrdinal)
	taskShells := []JenkinsTaskShell{}
	taskShells = append(taskShells, JenkinsTaskShell{Command: commandBuilder(activity, step)})
	commandBuilders := JenkinsBuilder{TaskShells: taskShells}

	scm := JenkinsSCM{Class: "hudson.scm.NullSCM"}

	postBuildSctipt := stepFinishScript
	if step.Type == model.StepTypeSCM {
		scm = JenkinsSCM{
			Class:           "hudson.plugins.git.GitSCM",
			Plugin:          "git@3.3.1",
			ConfigVersion:   2,
			GitRepo:         step.Repository,
			GitCredentialId: step.GitUser,
			GitBranch:       step.Branch,
		}
		postBuildSctipt = stepSCMFinishScript
	}
	preSCMStep := PreSCMBuildStepsWrapper{
		Plugin:      "preSCMbuildstep@0.3",
		FailOnError: false,
		Command:     fmt.Sprintf(stepStartScript, url.QueryEscape(activityId), stageOrdinal, stepOrdinal),
	}

	//Step timeout settings, at least 3 minutes
	var timeoutWrapper *TimeoutWrapperPlugin
	if step.Timeout > 0 {
		timeoutWrapper = &TimeoutWrapperPlugin{
			Plugin: "build-timeout@1.18",
			Strategy: TimeoutStrategy{
				Class:          "hudson.plugins.build_timeout.impl.AbsoluteTimeOutStrategy",
				TimeoutMinutes: step.Timeout,
			},
			Operation: "",
		}
	}

	v := &JenkinsProject{
		Scm:          scm,
		AssignedNode: activity.NodeName,
		CanRoam:      false,
		Disabled:     false,
		BlockBuildWhenDownstreamBuilding: false,
		BlockBuildWhenUpstreamBuilding:   false,
		CustomWorkspace:                  workspaceName,
		Builders:                         commandBuilders,
		TimeStampWrapper:                 TimestampWrapperPlugin{Plugin: "timestamper@1.8.8"},
		TimeoutWrapper:                   timeoutWrapper,
		PreSCMBuildStepsWrapper:          preSCMStep,
	}
	//post task to notify pipelineserver
	pbt := PostBuildTask{
		Plugin:             "groovy-postbuild@2.3.1",
		Behavior:           0,
		RunForMatrixParent: false,
		GroovyScript: GroovyScript{
			Plugin:  "script-security@1.30",
			Sandbox: false,
			Script:  fmt.Sprintf(postBuildSctipt, url.QueryEscape(activity.Id), stageOrdinal, stepOrdinal),
		},
	}
	v.Publishers = pbt

	return v

}

func (j JenkinsProvider) Reset() error {
	//TODO cleanup
	return nil
}

func commandBuilder(activity *model.Activity, step *model.Step) string {
	stringBuilder := new(bytes.Buffer)
	stringBuilder.WriteString("set +x \n")
	switch step.Type {
	case model.StepTypeTask:

		envVars := ""
		if len(step.Env) > 0 {
			for _, para := range step.Env {
				envVars += fmt.Sprintf("-e %s ", QuoteShell(para))
			}
		}

		entrypointPara := ""
		argsPara := ""
		svcPara := ""
		svcCheck := ""
		labelPara := fmt.Sprintf("-l activityid=%s", activity.Id)
		if step.ShellScript != "" {
			entrypointPara = "--entrypoint /bin/sh"
			entryFileName := fmt.Sprintf(".r_cicd_entrypoint_%s.sh", util.RandStringRunes(4))
			argsPara = entryFileName

			//write to a sh file,then docker run it
			stringBuilder.WriteString(fmt.Sprintf("cat>%s<<R_CICD_EOF\n", entryFileName))
			stringBuilder.WriteString("set -xe\n")
			cmd := strings.Replace(step.ShellScript, "\\", "\\\\", -1)
			cmd = strings.Replace(cmd, "$", "\\$", -1)
			stringBuilder.WriteString(cmd)
			stringBuilder.WriteString("\nR_CICD_EOF\n")
		} else {
			if step.Entrypoint != "" {
				entrypointPara = "--entrypoint " + step.Entrypoint
			}
			argsPara = step.Args
		}
		stringBuilder.WriteString(". ${PWD}/.r_cicd.env\n")
		//isService
		if step.IsService {
			containerName := activity.Id + step.Alias
			svcPara = "-itd --name " + containerName
			svcCheck = fmt.Sprintf("\necho 'run a service container with alias %s.'", step.Alias)
			svcCheck = svcCheck + fmt.Sprintf("\nsleep 3;if [ \"$(docker inspect -f {{.State.Running}} %s)\" = \"false\" ];then docker logs \"%s\";echo \"Error: service container \\\"%s\\\" is stopped.\ncheck above logs or the task step config.\nA running container is expected when using \\\"as a service\\\" option.\";exit 1;fi", containerName, containerName, step.Alias)
		}

		//add link service
		linkInfo := ""
		if len(step.Services) > 0 {
			linkInfo += ""
			for _, svc := range step.Services {
				linkInfo += fmt.Sprintf("--link %s:%s ", svc.ContainerName, svc.Name)
			}

		}

		volumeInfo := "--volumes-from ${HOSTNAME} -w ${PWD}"
		//volumeInfo := "-v /var/jenkins_home/workspace:/var/jenkins_home/workspace -w ${PWD}"
		stringBuilder.WriteString("docker run --rm")
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString("--env-file ${PWD}/.r_cicd.env")
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(envVars)
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(labelPara)
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(svcPara)
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(volumeInfo)
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(entrypointPara)
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(linkInfo)
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(step.Image)
		stringBuilder.WriteString(" ")
		stringBuilder.WriteString(argsPara)
		stringBuilder.WriteString(svcCheck)
	case model.StepTypeBuild:
		stringBuilder.WriteString(". ${PWD}/.r_cicd.env\n")
		if step.Dockerfile == "" {
			buildPath := "."
			if step.BuildPath != "" {
				buildPath = step.BuildPath
			}
			dockerfilePath := "Dockerfile"
			if step.DockerfilePath != "" {
				dockerfilePath = step.DockerfilePath
			}
			stringBuilder.WriteString("set -xe\n")
			stringBuilder.WriteString("docker build --tag ")
			stringBuilder.WriteString(QuoteShell(step.TargetImage))
			stringBuilder.WriteString(" ")
			stringBuilder.WriteString("-f " + QuoteShell(dockerfilePath))
			stringBuilder.WriteString(" ")
			stringBuilder.WriteString(QuoteShell(buildPath))
			stringBuilder.WriteString(";")
		} else {
			stringBuilder.WriteString("echo " + QuoteShell(step.Dockerfile) + ">.r_cicd_Dockerfile;\n")
			stringBuilder.WriteString("set -xe\n")
			stringBuilder.WriteString("docker build --tag ")
			stringBuilder.WriteString(step.TargetImage)
			stringBuilder.WriteString(" -f .r_cicd_Dockerfile .;")
		}
		if step.PushFlag {
			stringBuilder.WriteString("\ncihelper pushimage ")
			stringBuilder.WriteString(step.TargetImage)
			stringBuilder.WriteString(";")
		}
	case model.StepTypeSCM:
		//write to a env file that provides the environment variables to use throughout the activity.
		stringBuilder.WriteString("GIT_BRANCH=$(echo $GIT_BRANCH|cut -d / -f 2)\n")
		stringBuilder.WriteString("cat>.r_cicd.env<<R_CICD_EOF\n")
		stringBuilder.WriteString("CICD_GIT_COMMIT=$GIT_COMMIT\n")
		stringBuilder.WriteString("CICD_GIT_BRANCH=$GIT_BRANCH\n")
		stringBuilder.WriteString("CICD_GIT_URL=$GIT_URL\n")
		stringBuilder.WriteString("CICD_PIPELINE_NAME=" + activity.Pipeline.Name + "\n")
		stringBuilder.WriteString("CICD_PIPELINE_ID=" + activity.Pipeline.Id + "\n")
		stringBuilder.WriteString("CICD_TRIGGER_TYPE=" + activity.TriggerType + "\n")
		stringBuilder.WriteString("CICD_NODE_NAME=" + activity.NodeName + "\n")
		stringBuilder.WriteString("CICD_ACTIVITY_ID=" + activity.Id + "\n")
		stringBuilder.WriteString("CICD_ACTIVITY_SEQUENCE=" + strconv.Itoa(activity.RunSequence) + "\n")
		//user defined env vars
		for _, envvar := range activity.Pipeline.Parameters {
			splits := strings.SplitN(envvar, "=", 2)
			if len(splits) != 2 {
				continue
			}
			stringBuilder.WriteString(fmt.Sprintf("%s=%s\n", splits[0], QuoteShell(splits[1])))
		}
		stringBuilder.WriteString("\nR_CICD_EOF\n")

	case model.StepTypeUpgradeService:
		stringBuilder.WriteString(". ${PWD}/.r_cicd.env\n")
		stringBuilder.WriteString("cihelper")
		if step.Endpoint != "" {
			stringBuilder.WriteString(" --envurl ")
			stringBuilder.WriteString(QuoteShell(step.Endpoint))
			stringBuilder.WriteString(" --accesskey ")
			stringBuilder.WriteString(QuoteShell(step.Accesskey))
			stringBuilder.WriteString(" --secretkey ")
			envKey, err := service.GetEnvKey(step.Accesskey)
			if err != nil {
				logrus.Errorf("error get env credential:%v", err)
			}
			stringBuilder.WriteString(QuoteShell(envKey))
		} else {
			//read from env var
			stringBuilder.WriteString(" --envurl $CATTLE_URL")
			stringBuilder.WriteString(" --accesskey $CATTLE_ACCESS_KEY")
			stringBuilder.WriteString(" --secretkey $CATTLE_SECRET_KEY")
		}
		stringBuilder.WriteString(" upgrade service ")
		if step.ImageTag != "" {
			stringBuilder.WriteString(" --image ")
			stringBuilder.WriteString(step.ImageTag)
		}
		for k, v := range step.ServiceSelector {
			stringBuilder.WriteString(" --selector ")
			stringBuilder.WriteString(QuoteShell(fmt.Sprintf("%s=%s", k, v)))
		}
		if step.BatchSize > 0 {
			stringBuilder.WriteString(" --batchsize ")
			stringBuilder.WriteString(strconv.Itoa(step.BatchSize))
		}
		if step.Interval != 0 {
			stringBuilder.WriteString(" --interval ")
			stringBuilder.WriteString(strconv.Itoa(step.Interval))
		}
		if step.StartFirst != false {
			stringBuilder.WriteString(" --startfirst")
			stringBuilder.WriteString(" true")
		}
	case model.StepTypeUpgradeStack:
		stringBuilder.WriteString(". ${PWD}/.r_cicd.env\n")
		if step.Endpoint == "" {
			script := fmt.Sprintf(upgradeStackScript, "$CATTLE_URL", "$CATTLE_ACCESS_KEY", "$CATTLE_SECRET_KEY", step.StackName, EscapeShell(activity, step.DockerCompose), EscapeShell(activity, step.RancherCompose))
			stringBuilder.WriteString(script)
		} else {
			envKey, err := service.GetEnvKey(step.Accesskey)
			if err != nil {
				logrus.Errorf("error get env credential:%v", err)
			}
			script := fmt.Sprintf(upgradeStackScript, step.Endpoint, step.Accesskey, envKey, step.StackName, EscapeShell(activity, step.DockerCompose), EscapeShell(activity, step.RancherCompose))
			stringBuilder.WriteString(script)
		}
	case model.StepTypeUpgradeCatalog:
		stringBuilder.WriteString(". ${PWD}/.r_cicd.env\n")

		_, templateName, templateBase, _, _ := templateURLPath(step.ExternalId)

		systemFlag := ""
		if templateBase != "" {
			systemFlag = "--system "
		}
		deployFlag := ""
		if step.DeployFlag {
			deployFlag = "true"
		}

		dockerCompose := ""
		rancherCompose := ""
		readme := ""
		for k, v := range step.Templates {
			if strings.HasPrefix(k, "docker-compose") {
				dockerCompose = v
			} else if strings.HasPrefix(k, "rancher-compose") {
				rancherCompose = v
			} else if k == "README.md" {
				readme = v
			}

		}
		dockerCompose = EscapeShell(activity, dockerCompose)
		rancherCompose = EscapeShell(activity, rancherCompose)
		readme = EscapeShell(activity, readme)
		answers := EscapeShell(activity, step.Answers)

		var endpoint string
		var accessKey string
		var envKey string
		var err error
		if step.Endpoint != "" {
			endpoint = step.Endpoint
			accessKey = step.Accesskey
			envKey, err = service.GetEnvKey(step.Accesskey)
			if err != nil {
				logrus.Errorf("error get env credential:%v", err)
			}
		} else {
			endpoint = "$CATTLE_URL"
			accessKey = "$CATTLE_ACCESS_KEY"
			envKey = "$CATTLE_SECRET_KEY"
		}

		gitUserName := activity.Pipeline.Stages[0].Steps[0].GitUser
		script := fmt.Sprintf(upgradeCatalogScript, step.Repository, step.Branch, gitUserName, systemFlag, templateName, deployFlag, dockerCompose, rancherCompose, readme, answers, endpoint, accessKey, envKey, step.StackName)
		stringBuilder.WriteString(script)
	}

	return stringBuilder.String()
}

func (j JenkinsProvider) SyncActivity(activity *model.Activity) error {
	for i, actiStage := range activity.ActivityStages {
		for j, actiStep := range actiStage.ActivitySteps {
			if actiStep.Status == model.ActivityStepFail || actiStep.Status == model.ActivityStepSuccess {
				continue
			}
			jobName := getJobName(activity, i, j)
			jobInfo, err := GetJobInfo(jobName)
			if err != nil {
				//cannot get jobinfo
				logrus.Debugf("got job info:%v,err:%v", jobInfo, err)
				return err
			}

			buildInfo, err := GetBuildInfo(jobName)
			if err != nil {
				if actiStage.NeedApproval && j == 0 {
					//Pending
					actiStage.Status = model.ActivityStagePending
					activity.Status = model.ActivityPending
				}
				break
			}

			if err == nil {
				if buildInfo.Result == "SUCCESS" {
					actiStep.StartTS = buildInfo.Timestamp
					actiStep.Duration = buildInfo.Duration
					actiStep.Status = model.ActivityStepSuccess
					if j == len(actiStage.ActivitySteps)-1 {
						//Stage Success
						actiStage.Status = model.ActivityStageSuccess
						actiStage.Duration = buildInfo.Timestamp + buildInfo.Duration - actiStage.StartTS
					}
				} else if buildInfo.Result == "FAILURE" {
					actiStep.StartTS = buildInfo.Timestamp
					actiStep.Duration = buildInfo.Duration
					actiStep.Status = model.ActivityStepFail
					//Stage Fail
					actiStage.Status = model.ActivityStageFail
					actiStage.Duration = buildInfo.Timestamp + buildInfo.Duration - actiStage.StartTS
					//Activity Fail
					activity.Status = model.ActivityFail
					activity.StopTS = buildInfo.Timestamp + buildInfo.Duration
				} else if buildInfo.Building {
					//Building
					actiStep.StartTS = buildInfo.Timestamp
					actiStep.Status = model.ActivityStepBuilding
					actiStage.Status = model.ActivityStageBuilding
					activity.Status = model.ActivityBuilding
					break
				}

			}

		}
	}
	return nil
}

//SyncActivity gets latest activity info, return true if status if changed
func (j JenkinsProvider) SyncActivityStale(activity *model.Activity) (bool, error) {
	p := activity.Pipeline
	var updated bool

	//logrus.Infof("syncing activity:%v", activity.Id)
	//logrus.Infof("activity is:%v", activity)
	for i, actiStage := range activity.ActivityStages {
		jobName := p.Name + "_" + actiStage.Name + "_" + activity.Id
		beforeStatus := actiStage.Status

		if beforeStatus == model.ActivityStageSuccess {
			continue
		}

		jobInfo, err := GetJobInfo(jobName)
		if err != nil {
			//cannot get jobinfo
			logrus.Infof("got job info:%v,err:%v", jobInfo, err)
			return false, err
		}

		/*
			if (jobInfo.LastBuild == JenkinsJobInfo.LastBuild{}) {
				//no build finish
				return nil
			}
		*/
		buildInfo, err := GetBuildInfo(jobName)
		//logrus.Infof("got build info:%v, err:%v", buildInfo, err)
		if err != nil {
			//cannot get build info
			//build not started
			if actiStage.Status == model.ActivityStagePending {
				return updated, nil
			}
			actiStage.Status = model.ActivityStageWaiting
			break
		}
		getCommit(activity, buildInfo)
		//if any buildInfo found,activity in building status
		activity.Status = model.ActivityBuilding
		actiStage.Status = model.ActivityStageBuilding
		actiStage.StartTS = buildInfo.Timestamp

		//logrus.Info("get buildinfo result:%v,actiStagestatus:%v", buildInfo.Result, actiStage.Status)
		if err == nil {
			rawOutput, err := GetBuildRawOutput(jobName, 0)
			if err != nil {
				logrus.Infof("got rawOutput:%v,err:%v", rawOutput, err)
			}
			//actiStage.RawOutput = rawOutput
			stepStatusUpdated := parseSteps(actiStage, rawOutput)

			if actiStage.Status == model.ActivityStageFail {
				activity.Status = model.ActivityFail
				updated = true
			} else if actiStage.Status == model.ActivityStageSuccess {
				if i == len(p.Stages)-1 {
					//if all stage success , mark activity as success
					activity.StopTS = buildInfo.Timestamp + buildInfo.Duration
					activity.Status = model.ActivitySuccess
					updated = true
				}
				logrus.Infof("stage success:%v", i)

				if i < len(p.Stages)-1 && activity.Pipeline.Stages[i+1].NeedApprove {
					logrus.Infof("set pending")
					activity.Status = model.ActivityPending
					activity.ActivityStages[i+1].Status = model.ActivityStagePending
					activity.PendingStage = i + 1
				}
			}
			updated = updated || stepStatusUpdated
		}
		if beforeStatus != actiStage.Status {
			updated = true
			logrus.Infof("sync activity %v,updated !", activity.Id)
		}
		logrus.Debugf("after sync,beforestatus and after:%v,%v", beforeStatus, actiStage.Status)
	}

	return updated, nil
}

//OnActivityCompelte helps clean up
func (j JenkinsProvider) OnActivityCompelte(activity *model.Activity) {
	//clean related container by label
	command := fmt.Sprintf("docker ps --filter label=activityid=%s -q | xargs docker rm -f", activity.Id)
	cleanServiceScript := fmt.Sprintf(ScriptSkel, activity.NodeName, strings.Replace(command, "\"", "\\\"", -1))
	logrus.Debugf("cleanservicescript is: %v", cleanServiceScript)
	res, err := ExecScript(cleanServiceScript)
	logrus.Debugf("clean services result:%v,%v", res, err)
	if err != nil {
		logrus.Errorf("error cleanning up on worker node: %v, got result '%s'", err, res)
	}
	logrus.Infof("activity '%s' complete", activity.Id)
	//clean workspace
	if !activity.Pipeline.KeepWorkspace {
		command = "rm -rf ${System.getenv('JENKINS_HOME')}/workspace/" + activity.Id
		cleanWorkspaceScript := fmt.Sprintf(ScriptSkel, activity.NodeName, strings.Replace(command, "\"", "\\\"", -1))
		res, err = ExecScript(cleanWorkspaceScript)
		if err != nil {
			logrus.Errorf("error cleanning up on worker node: %v, got result '%s'", err, res)
		}
		logrus.Debugf("clean workspace result:%v,%v", res, err)
	}

}

func (j JenkinsProvider) OnCreateAccount(account *model.GitAccount) error {
	jenkinsCred := &JenkinsCredential{}
	jenkinsCred.Class = "com.cloudbees.plugins.credentials.impl.UsernamePasswordCredentialsImpl"
	jenkinsCred.Scope = "GLOBAL"
	jenkinsCred.Id = account.Id
	if account.AccountType == "github" {
		jenkinsCred.Username = account.Login
		jenkinsCred.Password = account.AccessToken
	} else if account.AccountType == "gitlab" {
		jenkinsCred.Username = "oauth2"
		jenkinsCred.Password = account.AccessToken
	} else {
		return errors.New("unknown scmtype")
	}
	bodyContent := map[string]interface{}{}
	bodyContent["credentials"] = jenkinsCred
	b, err := json.Marshal(bodyContent)
	if err != nil {
		return err
	}
	buff := bytes.NewBufferString("json=")
	buff.Write(b)
	if err := CreateCredential(buff.Bytes()); err != nil {
		return err
	}
	return nil
}

func (j JenkinsProvider) OnDeleteAccount(account *model.GitAccount) error {
	if account == nil {
		return errors.New("nil account")
	}
	return DeleteCredential(account.Id)
}

func (j JenkinsProvider) GetStepLog(activity *model.Activity, stageOrdinal int, stepOrdinal int, paras map[string]interface{}) (string, error) {
	if stageOrdinal < 0 || stageOrdinal >= len(activity.ActivityStages) || stepOrdinal < 0 || stepOrdinal >= len(activity.ActivityStages[stageOrdinal].ActivitySteps) {
		return "", errors.New("ordinal out of range")
	}
	jobName := getJobName(activity, stageOrdinal, stepOrdinal)
	var logText *string
	if val, ok := paras["prevLog"]; ok {
		logText = val.(*string)
	}
	startLine := len(strings.Split(*logText, "\n"))

	rawOutput, err := GetBuildRawOutput(jobName, startLine)
	if err != nil {
		return "", err
	}
	token := "\\n\\w{14}\\s{2}\\[.*?\\].*?\\.sh"
	*logText = *logText + rawOutput
	outputs := regexp.MustCompile(token).Split(*logText, -1)
	if len(outputs) > 1 && stageOrdinal == 0 && stepOrdinal == 0 {
		// SCM
		return trimFirstLine(outputs[1]), nil
	}
	if len(outputs) < 3 {
		//no printed log
		return "", nil
	}
	//hide set +x
	return trimFirstLine(outputs[2]), nil

}

func getCommit(activity *model.Activity, buildInfo *JenkinsBuildInfo) {
	if activity.CommitInfo != "" {
		return
	}

	logrus.Debugf("try to get commitInfo,action:%v", buildInfo.Actions)
	actions := buildInfo.Actions
	for _, action := range actions {

		logrus.Debugf("lastbuiltrevision:%v", action.LastBuiltRevision.SHA1)
		if action.LastBuiltRevision.SHA1 != "" {
			activity.CommitInfo = action.LastBuiltRevision.SHA1
		}
	}
}

//parse jenkins rawoutput to steps,return true if status updated
func parseSteps(actiStage *model.ActivityStage, rawOutput string) bool {
	token := "\\n\\w{14}\\s{2}\\[.*?\\].*?\\.sh"
	lastStatus := model.ActivityStepBuilding
	var updated bool = false
	if strings.HasSuffix(rawOutput, "  Finished: SUCCESS\n") {
		lastStatus = model.ActivityStepSuccess
		actiStage.Status = model.ActivityStageSuccess
	} else if strings.HasSuffix(rawOutput, "  Finished: FAILURE\n") {
		lastStatus = model.ActivityStepFail
		actiStage.Status = model.ActivityStageFail
	}
	outputs := regexp.MustCompile(token).Split(rawOutput, -1)
	//logrus.Infof("split to %v parts,steps number:%v, parse outputs:%v", len(outputs), len(actiStage.ActivitySteps), outputs)
	if len(outputs) > 0 && len(actiStage.ActivitySteps) > 0 && strings.Contains(outputs[0], "  Cloning the remote Git repository\n") {
		// SCM
		//actiStage.ActivitySteps[0].Message = outputs[0]
		if actiStage.ActivitySteps[0].Status != lastStatus {
			updated = true
			actiStage.ActivitySteps[0].Status = lastStatus
		}
		//get step time for SCM
		parseStepTime(actiStage.ActivitySteps[0], outputs[0], actiStage.StartTS)
		actiStage.Duration = actiStage.ActivitySteps[0].Duration
		return updated
	}
	logrus.Debugf("parsed,len output:%v", len(outputs))
	stageTime := int64(0)
	for i, step := range actiStage.ActivitySteps {
		finishStepNum := len(outputs) - 1
		prevStatus := step.Status
		logrus.Debug("getting step %v", i)
		if i < finishStepNum-1 {
			//passed steps
			step.Status = model.ActivityStepSuccess
			parseStepTime(step, outputs[i+1], actiStage.StartTS)
			stageTime = stageTime + step.Duration
		} else if i == finishStepNum-1 {
			//last run step
			step.Status = lastStatus
			parseStepTime(step, outputs[i+1], actiStage.StartTS)
			stageTime = stageTime + step.Duration
		} else {
			//not run steps
			step.Status = model.ActivityStepWaiting
		}
		if prevStatus != step.Status {
			updated = true
		}
		actiStage.ActivitySteps[i] = step
		logrus.Debugf("now step is %v.", step)
	}
	actiStage.Duration = stageTime
	logrus.Debugf("now actistage is %v.", actiStage)

	return updated

}

func parseStepTime(step *model.ActivityStep, log string, activityStartTS int64) {
	logrus.Infof("parsesteptime")
	token := "(^|\\n)\\w{14}  "
	r, _ := regexp.Compile(token)
	lines := r.FindAllString(log, -1)
	if len(lines) == 0 {
		return
	}

	start := strings.TrimLeft(lines[0], "\n")
	start = strings.TrimRight(start, " ")
	durationStart, err := time.ParseDuration(start)
	if err != nil {
		logrus.Errorf("parse duration error!%v", err)
		return
	}

	//compute step duration when done
	step.StartTS = activityStartTS + (durationStart.Nanoseconds() / int64(time.Millisecond))

	if step.Status != model.ActivityStepSuccess && step.Status != model.ActivityStepFail {
		return
	}

	end := strings.TrimLeft(lines[len(lines)-1], "\n")
	end = strings.TrimRight(end, " ")
	durationEnd, err := time.ParseDuration(end)
	if err != nil {
		logrus.Errorf("parse duration error!%vparseStepTime", err)
		return
	}
	duration := (durationEnd.Nanoseconds() - durationStart.Nanoseconds()) / int64(time.Millisecond)
	step.Duration = duration
}

//ToActivity init an activity from pipeline def
func ToActivity(p *model.Pipeline) (*model.Activity, error) {

	//Find a jenkins slave on which to run
	nodeName, err := getNodeNameToRun()
	if err != nil {
		return &model.Activity{}, err
	}
	activity := &model.Activity{
		Id:          uuid.Rand().Hex(),
		Pipeline:    *p,
		RunSequence: p.RunCount + 1,
		Status:      model.ActivityWaiting,
		StartTS:     time.Now().UnixNano() / int64(time.Millisecond),
		NodeName:    nodeName,
	}
	for _, stage := range p.Stages {
		activity.ActivityStages = append(activity.ActivityStages, ToActivityStage(stage))
	}

	return activity, nil
}

func initActivityEnvvars(activity *model.Activity) {
	p := activity.Pipeline
	vars := map[string]string{}
	vars["CICD_PIPELINE_NAME"] = p.Name
	vars["CICD_PIPELINE_ID"] = p.Id
	vars["CICD_NODE_NAME"] = activity.NodeName
	vars["CICD_ACTIVITY_ID"] = activity.Id
	vars["CICD_ACTIVITY_SEQUENCE"] = strconv.Itoa(activity.RunSequence)
	vars["CICD_GIT_URL"] = p.Stages[0].Steps[0].Repository
	vars["CICD_GIT_BRANCH"] = p.Stages[0].Steps[0].Branch
	vars["CICD_GIT_COMMIT"] = activity.CommitInfo
	vars["CICD_TRIGGER_TYPE"] = activity.TriggerType
	//user defined env vars
	for _, envvar := range activity.Pipeline.Parameters {
		splits := strings.SplitN(envvar, "=", 2)
		if len(splits) != 2 {
			continue
		}
		vars[splits[0]] = splits[1]
	}
	activity.EnvVars = vars
}

func ToActivityStage(stage *model.Stage) *model.ActivityStage {
	actiStage := model.ActivityStage{
		Name:          stage.Name,
		NeedApproval:  stage.NeedApprove,
		Status:        "Waiting",
		ActivitySteps: []*model.ActivityStep{},
	}
	for _, step := range stage.Steps {
		actiStep := &model.ActivityStep{
			Name:   step.Name,
			Status: model.ActivityStepWaiting,
		}
		actiStage.ActivitySteps = append(actiStage.ActivitySteps, actiStep)
	}
	return &actiStage

}

func QuoteShell(script string) string {
	//Use double quotes so variable substitution works

	escaped := strings.Replace(script, "\\", "\\\\", -1)
	escaped = strings.Replace(script, "\"", "\\\"", -1)
	escaped = "\"" + escaped + "\""
	return escaped
}

func EscapeShell(activity *model.Activity, script string) string {
	escaped := strings.Replace(script, "\\", "\\\\", -1)
	escaped = strings.Replace(escaped, "$", "\\$", -1)

	for k, _ := range activity.EnvVars {
		escaped = strings.Replace(escaped, "\\$"+k+" ", "$"+k+" ", -1)
		escaped = strings.Replace(escaped, "\\$"+k+"\n", "$"+k+"\n", -1)
		escaped = strings.Replace(escaped, "\\${"+k+"}", "${"+k+"}", -1)

	}
	return escaped
}

//merely substitute envvars without escaping shell
func SubstituteVar(activity *model.Activity, text string) string {
	for k, v := range activity.EnvVars {
		text = strings.Replace(text, "$"+k+" ", v, -1)
		text = strings.Replace(text, "$"+k+"\n", v, -1)
		text = strings.Replace(text, "${"+k+"}", v, -1)

	}
	return text
}

func templateURLPath(path string) (string, string, string, string, bool) {
	pathSplit := strings.Split(path, ":")
	switch len(pathSplit) {
	case 2:
		catalog := pathSplit[0]
		template := pathSplit[1]
		templateSplit := strings.Split(template, "*")
		templateBase := ""
		switch len(templateSplit) {
		case 1:
			template = templateSplit[0]
		case 2:
			templateBase = templateSplit[0]
			template = templateSplit[1]
		default:
			return "", "", "", "", false
		}
		return catalog, template, templateBase, "", true
	case 3:
		catalog := pathSplit[0]
		template := pathSplit[1]
		revisionOrVersion := pathSplit[2]
		templateSplit := strings.Split(template, "*")
		templateBase := ""
		switch len(templateSplit) {
		case 1:
			template = templateSplit[0]
		case 2:
			templateBase = templateSplit[0]
			template = templateSplit[1]
		default:
			return "", "", "", "", false
		}
		return catalog, template, templateBase, revisionOrVersion, true
	default:
		return "", "", "", "", false
	}
}

func getJobName(activity *model.Activity, stageOrdinal int, stepOrdinal int) string {
	stage := activity.ActivityStages[stageOrdinal]
	jobName := strings.Join([]string{activity.Pipeline.Name, activity.Id, stage.Name, strconv.Itoa(stepOrdinal)}, "_")
	return jobName
}

func getStageJobsName(activity *model.Activity, stageOrdinal int) string {
	stage := activity.ActivityStages[stageOrdinal]
	jobsName := []string{}
	for i := 0; i < len(stage.ActivitySteps); i++ {
		stepJobName := getJobName(activity, stageOrdinal, i)
		jobsName = append(jobsName, stepJobName)
	}
	return strings.Join(jobsName, ",")
}

func trimFirstLine(text string) string {
	text = strings.TrimLeft(text, "\n")
	splits := strings.SplitN(text, "\n", 2)
	if len(splits) != 2 {
		return ""
	}
	return splits[1]
}
