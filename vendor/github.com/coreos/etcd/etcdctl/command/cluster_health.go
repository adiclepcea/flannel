package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"time"

	"github.com/codegangsta/cli"
	"golang.org/x/net/context"
)

func NewClusterHealthCommand() cli.Command {
	return cli.Command{
		Name:  "cluster-health",
		Usage: "check the health of the etcd cluster",
		Flags: []cli.Flag{
			cli.BoolFlag{Name: "forever", Usage: "forever check the health every 10 second until CTRL+C"},
		},
		Action: handleClusterHealth,
	}
}

func handleClusterHealth(c *cli.Context) {
	forever := c.Bool("forever")
	if forever {
		sigch := make(chan os.Signal, 1)
		signal.Notify(sigch, os.Interrupt)

		go func() {
			<-sigch
			os.Exit(0)
		}()
	}

	tr, err := getTransport(c)
	if err != nil {
		handleError(ExitServerError, err)
	}

	// TODO: update members when forever is set.
	mi := mustNewMembersAPI(c)
	ms, err := mi.List(context.TODO())
	if err != nil {
		fmt.Println("cluster may be unhealthy: failed to list members")
		handleError(ExitServerError, err)
	}
	cl := make([]string, 0)
	for _, m := range ms {
		cl = append(cl, m.ClientURLs...)
	}

	for {
		// check the /health endpoint of all members first

		ep, rs0, err := getLeaderStatus(tr, cl)
		if err != nil {
			fmt.Println("cluster may be unhealthy: failed to connect", cl)
			if forever {
				time.Sleep(10 * time.Second)
				continue
			}
			os.Exit(1)
		}

		time.Sleep(time.Second)

		// are all the members makeing progress?
		_, rs1, err := getLeaderStatus(tr, []string{ep})
		if err != nil {
			fmt.Println("cluster is unhealthy")
			if forever {
				time.Sleep(10 * time.Second)
				continue
			}
			os.Exit(1)
		}

		if rs1.Commit > rs0.Commit {
			fmt.Printf("cluster is healthy: raft is making progress [commit index: %v->%v]\n", rs0.Commit, rs1.Commit)
		} else {
			fmt.Printf("cluster is unhealthy: raft is not making progress [commit index: %v]\n", rs0.Commit)
		}
		fmt.Printf("leader is %v\n", rs0.Lead)

		var prints []string

		for id, pr0 := range rs0.Progress {
			pr1, ok := rs1.Progress[id]
			if !ok {
				// TODO: forever should handle configuration change.
				fmt.Println("Cluster configuration changed during health checking. Please retry.")
				os.Exit(1)
			}
			if pr1.Match <= pr0.Match {
				prints = append(prints, fmt.Sprintf("member %s is unhealthy: raft is not making progress [match: %v->%v]\n", id, pr0.Match, pr1.Match))
			} else {
				prints = append(prints, fmt.Sprintf("member %s is healthy: raft is making progress [match: %v->%v]\n", id, pr0.Match, pr1.Match))
			}
		}

		sort.Strings(prints)
		for _, p := range prints {
			fmt.Print(p)
		}

		if !forever {
			return
		}

		time.Sleep(10 * time.Second)
	}
}

type raftStatus struct {
	ID        string `json:"id"`
	Term      uint64 `json:"term"`
	Vote      string `json:"vote"`
	Commit    uint64 `json:"commit"`
	Lead      string `json:"lead"`
	RaftState string `json:"raftState"`
	Progress  map[string]struct {
		Match uint64 `json:"match"`
		Next  uint64 `json:"next"`
		State string `json:"state"`
	} `json:"progress"`
}

type vars struct {
	RaftStatus raftStatus `json:"raft.status"`
}

func getLeaderStatus(tr *http.Transport, endpoints []string) (string, raftStatus, error) {
	// TODO: use new etcd client
	httpclient := http.Client{
		Transport: tr,
	}

	for _, ep := range endpoints {
		resp, err := httpclient.Get(ep + "/debug/vars")
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}

		vs := &vars{}
		d := json.NewDecoder(resp.Body)
		err = d.Decode(vs)
		if err != nil {
			continue
		}
		if vs.RaftStatus.Lead != vs.RaftStatus.ID {
			continue
		}
		return ep, vs.RaftStatus, nil
	}
	return "", raftStatus{}, errors.New("no leader")
}
