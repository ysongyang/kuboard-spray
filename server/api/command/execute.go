package command

import (
	"errors"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/eip-work/kuboard-spray/common"
	"github.com/eip-work/kuboard-spray/constants"
	"github.com/sirupsen/logrus"
)

type Execute struct {
	Cmd      string
	Args     func(string) []string
	Cluster  string
	mutex    *sync.Mutex
	Type     string
	PreExec  func(string) error
	PostExec func(ExecuteExitStatus) (string, error)
	Dir      string
	Env      []string
	R_Error  error
	R_Pid    string
}

func (execute *Execute) ToString(runDirPath string, pid string) string {
	result := "---"
	result += "\ncluster: " + execute.Cluster
	result += "\nhistory: " + runDirPath
	result += "\npid: " + pid
	result += "\ndir: " + execute.Dir
	result += "\ncmd: " + execute.Cmd
	result += "\nargs:"
	args := execute.Args(runDirPath)
	for _, arg := range args {
		result += "\n  - " + arg
	}
	result += "\nenv:"
	for _, env := range execute.Env {
		result += "\n - " + env
	}
	result += "\nshell: cd " + execute.Dir
	for _, env := range execute.Env {
		result += " && export \"" + env + "\""
	}
	result += " && " + execute.Cmd
	for _, arg := range args {
		result += " " + arg
	}
	return result
}

func (execute *Execute) Exec() error {
	execute.mutex = &sync.Mutex{}
	execute.mutex.Lock()
	go execute.exec()
	logrus.Trace("geting lock in main thread...")
	execute.mutex.Lock()
	logrus.Trace("Got response from exec: ", execute.R_Error)
	execute.mutex.Unlock()
	return execute.R_Error
}

func (execute *Execute) exec() {

	lockFile, err := LockCluster(execute.Cluster)
	if err != nil {
		execute.R_Error = err
		execute.mutex.Unlock()
		return
	}

	defer UnlockCluster(lockFile)

	pid := time.Now().Format("2006-01-02_15-04-05.999") + "_" + execute.Type
	historyPath := constants.GET_DATA_INVENTORY_DIR() + "/" + execute.Cluster + "/history"
	if err := common.CreateDirIfNotExists(historyPath); err != nil {
		execute.R_Error = errors.New("cannot create historyDir : " + historyPath + " : " + err.Error())
		execute.mutex.Unlock()
		return
	}

	runDirPath := constants.GET_DATA_INVENTORY_DIR() + "/" + execute.Cluster + "/history/" + pid
	if err := common.CreateDirIfNotExists(runDirPath); err != nil {
		execute.R_Error = errors.New("cannot create runDir : " + runDirPath + " : " + err.Error())
		execute.mutex.Unlock()
		return
	}

	if execute.PreExec != nil {
		if err := execute.PreExec(runDirPath); err != nil {
			execute.R_Error = errors.New("failed to prepare for the task : " + err.Error())
			execute.mutex.Unlock()
			return
		}
	}

	logFilePath := runDirPath + "/command.log"
	logFile, err := os.OpenFile(logFilePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		execute.R_Error = errors.New("cannot create logFile : " + logFilePath + " : " + err.Error())
		execute.mutex.Unlock()
		return
	}

	defer logFile.Sync()
	defer logFile.Close()

	cmd := exec.Command(execute.Cmd, execute.Args(runDirPath)...)

	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = execute.Dir
	cmd.Env = execute.Env

	if err := cmd.Start(); err != nil {
		execute.R_Error = errors.New("failed to start command " + cmd.String() + " : " + err.Error())
		os.Remove(runDirPath)
		execute.mutex.Unlock()
		return
	}

	logrus.Trace("started command " + cmd.String())
	ioutil.WriteFile(runDirPath+"/command.string", []byte(cmd.String()), 0666)
	ioutil.WriteFile(runDirPath+"/command.yaml", []byte(execute.ToString(runDirPath, pid)), 0666)

	if err := lockFile.Truncate(0); err != nil {
		execute.R_Error = errors.New("failed to truncate lockFile : " + err.Error())
	}
	_, err = lockFile.WriteString(pid)
	if err != nil {
		execute.R_Error = errors.New("failed to write pid " + err.Error())
	}

	execute.R_Pid = pid

	execute.mutex.Unlock()
	cmd.Wait()

	if execute.PostExec != nil {
		logFile.WriteString("\n\n\nKUBOARD SPRAY *****************************************************************\n")
		logs, err := ioutil.ReadFile(logFilePath)
		if err != nil {
			return
		}
		logStr := string(logs)
		recap := logStr[strings.LastIndex(logStr, "PLAY RECAP *********************************************************************")+81:]
		recap = recap[:strings.Index(recap, "\n\n")]

		lines := strings.Split(recap, "\n")
		status := []ExecuteExitNodeStatus{}
		for _, line := range lines {
			status = append(status, parseAnsibleRecapLine(line))
		}
		success := true
		for _, nodestatus := range status {
			if nodestatus.Unreachable != "0" || nodestatus.Failed != "0" {
				success = false
			}
		}

		exitStatus := ExecuteExitStatus{
			Success:    success,
			NodeStatus: status,
			Pid:        pid,
			RunDir:     runDirPath,
		}

		message, err := execute.PostExec(exitStatus)
		if err != nil {
			logFile.WriteString("Error in PostExec: " + err.Error() + "\n")
		}

		if success {
			task := SuccessTask{
				Type:      execute.Type,
				Timestamp: time.Now(),
				Pid:       pid,
			}
			if err := AddSuccessTask(execute.Cluster, task); err != nil {
				logrus.Warn("failed to add success task: ", err)
			}
		}

		logFile.WriteString(message)
	}
}

type ExecuteExitStatus struct {
	Success    bool
	NodeStatus []ExecuteExitNodeStatus
	Pid        string
	RunDir     string
}

type ExecuteExitNodeStatus struct {
	NodeName    string
	OK          string
	Changed     string
	Unreachable string
	Failed      string
	Skipped     string
	Rescued     string
	Ignored     string
}

func parseAnsibleRecapLine(line string) ExecuteExitNodeStatus {
	result := make(map[string]string)
	s1 := strings.Split(line, ":")

	result["node"] = strings.Trim(s1[0], " ")
	s2 := strings.Split(s1[1], " ")
	for _, kv := range s2 {
		s3 := strings.Split(kv, "=")
		if len(s3) == 1 {
			continue
		}
		k := strings.Trim(s3[0], " ")
		v := strings.Trim(s3[1], " ")
		result[k] = v
	}
	nodeStatus := ExecuteExitNodeStatus{
		NodeName:    result["node"],
		OK:          result["ok"],
		Changed:     result["changed"],
		Unreachable: result["unreachable"],
		Failed:      result["failed"],
		Skipped:     result["skipped"],
		Rescued:     result["rescured"],
		Ignored:     result["ignored"],
	}
	return nodeStatus
}
