package main

import (
	"os"

	"github.com/Sirupsen/logrus"
	_ "github.com/go-sql-driver/mysql"
	"github.com/rancher/pipeline/config"
	"github.com/rancher/pipeline/provider/jenkins"
	"github.com/rancher/pipeline/server"
	"github.com/urfave/cli"
)

var VERSION = "v0.0.0-dev"

func main() {
	app := cli.NewApp()
	app.Name = "pipeline"
	app.Version = VERSION
	app.Usage = "You need help!"
	app.Action = checkAndRun
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "jenkins_user",
			Usage:  "User of jenkins admin",
			EnvVar: "JENKINS_USER",
		},
		cli.StringFlag{
			Name:   "jenkins_token",
			Usage:  "token of jenkins admin",
			EnvVar: "JENKINS_TOKEN",
		},
		cli.StringFlag{
			Name:   "jenkins_address",
			Usage:  "token of jenkins admin",
			EnvVar: "JENKINS_ADDRESS",
			Value:  "http://jenkins:8080",
		},
		cli.StringFlag{
			Name:   "cattle_url",
			Usage:  "rancher server api address",
			EnvVar: "CATTLE_URL",
			Value:  "",
		},
		cli.StringFlag{
			Name:   "cattle_access_key",
			Usage:  "cattle access key",
			EnvVar: "CATTLE_ACCESS_KEY",
			Value:  "",
		},
		cli.StringFlag{
			Name:   "cattle_secret_key",
			Usage:  "cattle secret key",
			EnvVar: "CATTLE_SECRET_KEY",
			Value:  "",
		},
		cli.BoolFlag{
			Name:   "debug",
			Usage:  "enable debug mode",
			EnvVar: "DEBUG",
		},
	}
	app.Run(os.Args)
}

func checkAndRun(c *cli.Context) (rtnerr error) {
	if c.GlobalBool("debug") {
		logrus.SetLevel(logrus.DebugLevel)
	}
	config.Parse(c)
	jenkins.InitJenkins()
	provider := jenkins.JenkinsProvider{}
	errChan := make(chan bool)
	go server.ListenAndServe(provider, errChan)

	<-errChan
	logrus.Info("Going down")
	return nil
}
