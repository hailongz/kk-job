package slave

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hailongz/kk-job/app"
	"github.com/hailongz/kk-lib/dynamic"
	"github.com/hailongz/kk-micro/micro"
)

type ISlave interface {
	Run()
}

type Slave struct {
	workdir  string
	remote   micro.IRemote
	token    string
	maxCount int
}

func NewSlave(workdir string, baseURL string, token string, maxCount int) ISlave {
	v := Slave{}
	v.workdir = workdir
	v.remote = micro.NewHttpRemote(baseURL)
	v.token = token
	v.maxCount = maxCount
	return &v
}

type slaveWriter struct {
	slave        *Slave
	iid          int64
	fd           *os.File
	line         *bytes.Buffer
	securityKeys map[string]string
}

func newSlaveWriter(slave *Slave, iid int64, path string, securityKeys map[string]string) (*slaveWriter, error) {
	v := slaveWriter{}
	v.slave = slave
	v.iid = iid

	var err error = nil

	v.fd, err = os.OpenFile(path, os.O_APPEND, 0)

	if err != nil {

		v.fd, err = os.Create(path)

		if err != nil {
			return nil, err
		}

		os.Chmod(path, 0777)

	}

	return &v, err
}

func (L *slaveWriter) Close() error {
	return L.fd.Close()
}

func (L *slaveWriter) Write(p []byte) (n int, err error) {

	for _, c := range p {
		if c == '\n' {

			v := L.line.String()

			for _, value := range L.securityKeys {
				v = strings.Replace(v, value, "******", -1)
			}

			task := SlaveJobLogTask{}
			task.Token = L.slave.token
			task.Message = v
			task.Iid = L.iid

			err := L.slave.remote.Handle(&task)

			if err != nil {
				log.Println("[JOB] [LOG] [ERROR]", err)
			}

			L.line.Reset()
		} else {
			L.line.WriteByte(c)
		}
	}

	return L.fd.Write(p)
}

func (S *Slave) Run() {

	ch := make(chan SlaveJobItemTaskResult, S.maxCount)

	defer close(ch)

	go func() {

		for {

			task := SlaveLoginTask{}
			task.Token = S.token
			task.Platform = runtime.GOOS

			err := S.remote.Handle(&task)

			if err != nil {
				log.Println("[LOGIN] [ERROR]", err)
				time.Sleep(6 + time.Second)
				continue
			}

			break
		}

		for {

			task := SlaveJobItemTask{}
			task.Token = S.token

			err := S.remote.Handle(&task)

			if err != nil {
				log.Println("[JOB] [ERROR]", err)
				time.Sleep(6 + time.Second)
				continue
			}

			ch <- task.Result
		}

	}()

	for {

		r := <-ch

		go func(r SlaveJobItemTaskResult) {

			path := filepath.Join(S.workdir, strconv.FormatInt(r.Job.Id, 10), strconv.Itoa(r.JobItem.Version))

			log.Printf("[JOB] [%d] [RUN] %s-%s %s\n", r.JobItem.Id, r.Job.Title, r.JobItem.Title, path)

			err := func() error {

				err := os.MkdirAll(path, 0777)

				if err != nil {
					return err
				}

				code := bytes.NewBuffer(nil)

				env := map[string]string{
					"WORKDIR":     S.workdir,
					"JOB_ID":      strconv.FormatInt(r.Job.Id, 10),
					"JOB_VERSION": strconv.Itoa(r.JobItem.Version),
					"JOB_TITLE":   fmt.Sprintf("%s-%s", r.Job.Title, r.JobItem.Title),
				}

				securityKeys := map[string]string{}

				dynamic.Each(r.Job.Env, func(key interface{}, value interface{}) bool {

					skey := dynamic.StringValue(key, "")

					{
						s, ok := value.(string)
						if ok {
							env[skey] = s
							return true
						}
					}

					security := dynamic.BooleanValue(dynamic.Get(value, "security"), false)

					svalue := dynamic.StringValue(dynamic.Get(r.JobItem.Options, skey), "")

					if svalue == "" {
						svalue = dynamic.StringValue(dynamic.Get(value, "value"), "")
					}

					if security {
						securityKeys[skey] = svalue
					}

					env[skey] = svalue

					return true
				})

				for key, value := range env {
					code.WriteString(key)
					code.WriteString("=")
					b, _ := json.Marshal(value)
					code.Write(b)
					code.WriteString("\r\n")
				}

				code.WriteString(r.Job.Script)

				cmd := exec.Command("/bin/bash", "-c", code.String())

				cmd.Dir = path

				stdout, err := newSlaveWriter(S, r.JobItem.Id, filepath.Join(path, "info.log"), securityKeys)

				if err != nil {
					return err
				}

				cmd.Stderr = stdout
				cmd.Stdout = stdout

				defer stdout.Close()

				err = cmd.Start()

				if err != nil {
					return err
				}

				err = cmd.Wait()

				if err != nil {
					return err
				}

				return nil
			}()

			if err == nil {
				task := SlaveJobSetTask{}
				task.Token = S.token
				task.Status = app.JOB_ITEM_STATUS_OK
				err = S.remote.Handle(&task)
			} else {
				task := SlaveJobSetTask{}
				task.Token = S.token
				task.Status = app.JOB_ITEM_STATUS_ERROR
				S.remote.Handle(&task)
			}

			if err == nil {
				log.Printf("[JOB] [%d] [OK] %s-%s\n", r.JobItem.Id, r.Job.Title, r.JobItem.Title)
			} else {
				log.Printf("[JOB] [%d] [ERROR] %s-%s %s\n", r.JobItem.Id, r.Job.Title, r.JobItem.Title, err.Error())
			}

		}(r)
	}

}