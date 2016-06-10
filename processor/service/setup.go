package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	// "io"
	// "os"
	"strings"

	"github.com/nanobox-io/nanobox-boxfile"
	"github.com/nanobox-io/nanobox-golang-stylish"

	dockType "github.com/docker/engine-api/types"
	"github.com/nanobox-io/golang-docker-client"
	"github.com/nanobox-io/nanobox/models"
	"github.com/nanobox-io/nanobox/processor"
	"github.com/nanobox-io/nanobox/provider"
	"github.com/nanobox-io/nanobox/util"
	"github.com/nanobox-io/nanobox/util/data"
	"github.com/nanobox-io/nanobox/util/ip_control"
	"github.com/nanobox-io/nanobox/util/print"
)

type cleanFunc func() error

type serviceSetup struct {
	control     processor.ProcessControl
	service    	models.Service
	localIP   	net.IP
	globalIP  	net.IP
	container  	dockType.ContainerJSON
	plan       	string
	fail       	bool
	cleanFuncs 	[]cleanFunc
}

func init() {
	processor.Register("service_setup", serviceSetupFunc)
}

func serviceSetupFunc(control processor.ProcessControl) (processor.Processor, error) {
	// confirm the provider is an accessable one that we support.
	// ensure we have a name and immage
	if control.Meta["name"] == "" || control.Meta["image"] == "" {
		return nil, errors.New("missing image or name")
	}
	// add a label if im missing one
	if control.Meta["label"] == "" {
		control.Meta["label"] = control.Meta["name"]
	}

	return &serviceSetup{
		control:     control,
		cleanFuncs: make([]cleanFunc, 0),
	}, nil
}

// clean will iterate through the cleanup functions that were registered and
// call them one-by-one
func (self *serviceSetup) clean() error {
	// short-circuit if we haven't failed
	if !self.fail {
		return nil
	}

	// iterate through the cleanup functions that were registered and call them
	for _, cleanF := range self.cleanFuncs {
		if err := cleanF(); err != nil {
			return err
		}
	}

	return nil
}

func (self serviceSetup) Results() processor.ProcessControl {
	return self.control
}

func (self *serviceSetup) Process() error {

	header := fmt.Sprintf("Launching %s...", self.control.Meta["label"])
	self.control.Display(stylish.Bullet(header))

	// call the cleanup function to ensure we don't leave any bad state
	defer self.clean()

	if err := self.loadService(); err != nil {
		self.fail = true
		return err
	}

	// short-circuit if the service has already progressed past this point
	if self.service.State != "initialized" {
		return nil
	}

	if err := self.downloadImage(); err != nil {
		self.fail = true
		return err
	}

	if err := self.reserveIps(); err != nil {
		self.fail = true
		return err
	}

	if err := self.launchContainer(); err != nil {
		self.fail = true
		return err
	}

	if err := self.attachNetwork(); err != nil {
		self.fail = true
		return err
	}

	if err := self.planService(); err != nil {
		self.fail = true
		return err
	}

	if err := self.persistService(); err != nil {
		self.fail = true
		return err
	}

	if err := self.addEnvVars(); err != nil {
		self.fail = true
		return err
	}

	return nil
}

// validateMeta ensures we were given a name and image
func (self *serviceSetup) validateMeta() error {
	return nil
}

// loadService fetches the service from the database
func (self *serviceSetup) loadService() error {
	// the service really shouldn't exist yet, so let's not return the error if it fails
	data.Get(util.AppName(), self.control.Meta["name"], &self.service)

	// set the default state
	if self.service.State == "" {
		self.service.State = "initialized"
	}

	return nil
}

// downloadImage downloads the docker image
func (self *serviceSetup) downloadImage() error {
	// Create a pipe to for the JSONMessagesStream to read from
	// pr, pw := io.Pipe()
	prefix := fmt.Sprintf("%s+ Pulling %s -", stylish.GenerateNestedPrefix(self.control.DisplayLevel+1), self.control.Meta["image"])
	//  go print.DisplayJSONMessagesStream(pr, os.Stdout, os.Stdout.Fd(), true, prefix, nil)
	if _, err := docker.ImagePull(self.control.Meta["image"], &print.DockerPercentDisplay{Prefix: prefix}); err != nil {
		return err
	}

	return nil
}

// reserveIps reserves a global and local ip for the container
func (self *serviceSetup) reserveIps() error {
	var err error
	self.localIP, err = ip_control.ReserveLocal()
	if err != nil {
		return err
	}

	self.cleanFuncs = append(self.cleanFuncs, func() error {
		return ip_control.ReturnIP(self.localIP)
	})

	self.globalIP, err = ip_control.ReserveGlobal()
	if err != nil {
		return err
	}

	self.cleanFuncs = append(self.cleanFuncs, func() error {
		return ip_control.ReturnIP(self.globalIP)
	})

	return nil
}

// launchContainer launches and starts a docker container
func (self *serviceSetup) launchContainer() error {
	config := docker.ContainerConfig{
		Name:    fmt.Sprintf("nanobox-%s-%s", util.AppName(), self.control.Meta["name"]),
		Image:   self.control.Meta["image"],
		Network: "virt",
		IP:      self.localIP.String(),
	}

	self.control.Info(stylish.SubBullet("Starting container..."))
	container, err := docker.CreateContainer(config)
	if err != nil {
		return err
	}

	self.cleanFuncs = append(self.cleanFuncs, func() error {
		return docker.ContainerRemove(container.ID)
	})

	self.container = container

	return nil
}

// attachNetwork attaches the IP addresses to the container
func (self *serviceSetup) attachNetwork() error {
	label := "Bridging container to host network..."
	self.control.Info(stylish.SubBullet(label))

	err := provider.AddIP(self.globalIP.String())
	if err != nil {
		return err
	}

	self.cleanFuncs = append(self.cleanFuncs, func() error {
		return provider.RemoveIP(self.globalIP.String())
	})

	err = provider.AddNat(self.globalIP.String(), self.localIP.String())
	if err != nil {
		return err
	}

	self.cleanFuncs = append(self.cleanFuncs, func() error {
		return provider.RemoveNat(self.globalIP.String(), self.localIP.String())
	})

	return nil
}

// planService runs the plan hook
func (self *serviceSetup) planService() error {
	self.control.Info(stylish.SubBullet("Gathering service requirements..."))

	boxfile := boxfile.New([]byte(self.control.Meta["boxfile"]))
	boxConfig := boxfile.Node(self.control.Meta["name"]).Node("config")
	planPayload := map[string]interface{}{"config": boxConfig.Parsed}
	jsonPayload, _ := json.Marshal(planPayload)

	p, err := util.Exec(self.container.ID, "plan", string(jsonPayload), processor.ExecWriter())
	if err != nil {
		return err
	}
	self.plan = p

	return nil
}

// persistService saves the service in the database
func (self *serviceSetup) persistService() error {
	// save service in DB
	self.service.ID = self.container.ID
	self.service.Name = self.control.Meta["name"]
	self.service.ExternalIP = self.globalIP.String()
	self.service.InternalIP = self.localIP.String()
	self.service.State = "planned"
	self.service.Type = "data"

	err := json.Unmarshal([]byte(self.plan), &self.service.Plan)
	if err != nil {
		return fmt.Errorf("persistService:%s", err.Error())
	}
	for i := 0; i < len(self.service.Plan.Users); i++ {
		self.service.Plan.Users[i].Password = util.RandomString(10)
	}

	// save the service
	err = data.Put(util.AppName(), self.control.Meta["name"], &self.service)
	if err != nil {
		return err
	}

	return nil
}

// updateEnvVars will generate environment variables from the plan
func (self *serviceSetup) addEnvVars() error {
	// fetch the environment variables model
	envVars := models.EnvVars{}
	data.Get(util.AppName()+"_meta", "env", &envVars)

	// create a prefix for each of the environment variables.
	// for example, if the service is 'data.db' the prefix
	// would be DATA_DB. Dots are replaced with underscores,
	// and characters are uppercased.
	prefix := strings.ToUpper(strings.Replace(self.service.Name, ".", "_", -1))

	// we need to create an host evar that holds the IP of the service
	envVars[fmt.Sprintf("%s_HOST", prefix)] = self.service.InternalIP

	// we need to create evars that contain usernames and passwords
	//
	// during the plan phase, the service was informed of potentially
	// 	1 - users (all of the users)
	// 	2 - user (the default user)
	//
	// First, we need to create an evar that contains the list of users.
	// 	{prefix}_USERS
	//
	// Each user provided was given a password. For every user specified,
	// we need to create a corresponding evars to store the password:
	//  {prefix}_{username}_PASS
	//
	// Lastly, if a default user was provided, we need to create a pair
	// of environment variables as a convenience to the user:
	// 	1 - {prefix}_USER
	// 	2 - {prefix}_PASS

	// create a slice of user strings that we will use to generate the list of users
	users := []string{}

	// users will have been loaded into the service plan, so let's iterate
	for _, user := range self.service.Plan.Users {
		// add this username to the list
		users = append(users, user.Username)

		// generate the corresponding evar for the password
		key := fmt.Sprintf("%s_%s_PASS", prefix, strings.ToUpper(user.Username))
		envVars[key] = user.Password

		// if this user is the default user
		// set additional default env vars
		if user.Username == self.service.Plan.DefaultUser {
			envVars[fmt.Sprintf("%s_USER", prefix)] = user.Username
			envVars[fmt.Sprintf("%s_PASS", prefix)] = user.Password
		}
	}

	// if there are users, create an environment variable to represent the list
	if len(users) > 0 {
		envVars[fmt.Sprintf("%s_USERS", prefix)] = strings.Join(users, " ")
	}

	// persist the evars
	if err := data.Put(util.AppName()+"_meta", "env", envVars); err != nil {
		return err
	}

	return nil
}
