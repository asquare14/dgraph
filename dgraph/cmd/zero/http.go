/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package zero

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/golang/glog"
	"github.com/soheilhy/cmux"
)

// intFromQueryParam checks for name as a query param, converts it to uint64 and returns it.
// It also writes any errors to w. A bool is returned to indicate if the param was parsed
// successfully.
func intFromQueryParam(w http.ResponseWriter, r *http.Request, name string) (uint64, bool) {
	str := r.URL.Query().Get(name)
	if len(str) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidRequest, fmt.Sprintf("%s not passed", name))
		return 0, false
	}
	val, err := strconv.ParseUint(str, 0, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidRequest, fmt.Sprintf("Error while parsing %s", name))
		return 0, false
	}
	return val, true
}

func (st *state) assign(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return
	}
	val, ok := intFromQueryParam(w, r, "num")
	if !ok {
		return
	}

	num := &pb.Num{Val: uint64(val)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var ids *pb.AssignedIds
	var err error
	what := r.URL.Query().Get("what")
	switch what {
	case "uids":
		ids, err = st.zero.AssignUids(ctx, num)
	case "timestamps":
		if num.Val == 0 {
			num.ReadOnly = true
		}
		ids, err = st.zero.Timestamps(ctx, num)
	default:
		x.SetStatus(w, x.Error,
			fmt.Sprintf("Invalid what: [%s]. Must be one of uids or timestamps", what))
		return
	}
	if err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}

	m := jsonpb.Marshaler{EmitDefaults: true}
	if err := m.Marshal(w, ids); err != nil {
		x.SetStatus(w, x.ErrorNoData, err.Error())
		return
	}
}

// removeNode can be used to remove a node from the cluster. It takes in the RAFT id of the node
// and the group it belongs to. It can be used to remove Dgraph alpha and Zero nodes(group=0).
func (st *state) removeNode(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return
	}

	nodeId, ok := intFromQueryParam(w, r, "id")
	if !ok {
		return
	}
	groupId, ok := intFromQueryParam(w, r, "group")
	if !ok {
		return
	}

	if err := st.zero.removeNode(context.Background(), nodeId, uint32(groupId)); err != nil {
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	_, err := fmt.Fprintf(w, "Removed node with group: %v, idx: %v", groupId, nodeId)
	if err != nil {
		glog.Warningf("Error while writing response: %+v", err)
	}
}

// moveTablet can be used to move a tablet to a specific group. It takes in tablet and group as
// argument.
func (st *state) moveTablet(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return
	}

	if !st.node.AmLeader() {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidRequest,
			"This Zero server is not the leader. Re-run command on leader.")
		return
	}

	tablet := r.URL.Query().Get("tablet")
	if len(tablet) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidRequest, "tablet is a mandatory query parameter")
		return
	}

	groupId, ok := intFromQueryParam(w, r, "group")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidRequest, fmt.Sprintf(
			"Query parameter 'group' should contain a valid integer."))
		return
	}
	dstGroup := uint32(groupId)
	knownGroups := st.zero.KnownGroups()
	var isKnown bool
	for _, grp := range knownGroups {
		if grp == dstGroup {
			isKnown = true
			break
		}
	}
	if !isKnown {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidRequest, fmt.Sprintf("Group: [%d] is not a known group.",
			dstGroup))
		return
	}

	tab := st.zero.ServingTablet(tablet)
	if tab == nil {
		w.WriteHeader(http.StatusBadRequest)
		x.SetStatus(w, x.ErrorInvalidRequest, fmt.Sprintf("No tablet found for: %s", tablet))
		return
	}

	srcGroup := tab.GroupId
	if srcGroup == dstGroup {
		w.WriteHeader(http.StatusInternalServerError)
		x.SetStatus(w, x.ErrorInvalidRequest,
			fmt.Sprintf("Tablet: [%s] is already being served by group: [%d]", tablet, srcGroup))
		return
	}

	if err := st.zero.movePredicate(tablet, srcGroup, dstGroup); err != nil {
		glog.Errorf("While moving predicate %s from %d -> %d. Error: %v",
			tablet, srcGroup, dstGroup, err)
		w.WriteHeader(http.StatusInternalServerError)
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	_, err := fmt.Fprintf(w, "Predicate: [%s] moved from group [%d] to [%d]",
		tablet, srcGroup, dstGroup)
	if err != nil {
		glog.Warningf("Error while writing response: %+v", err)
	}
}

func (st *state) getState(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.node.WaitLinearizableRead(ctx); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	mstate := st.zero.membershipState()
	if mstate == nil {
		x.SetStatus(w, x.ErrorNoData, "No membership state found.")
		return
	}

	m := jsonpb.Marshaler{EmitDefaults: true}
	if err := m.Marshal(w, mstate); err != nil {
		x.SetStatus(w, x.ErrorNoData, err.Error())
		return
	}
}

func (st *state) pingResponse(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (st *state) startListenHttpAndHttps(l net.Listener) {
	if Zero.Conf.GetString("tls_dir") == "" && Zero.Conf.GetString("tls_disabled_route") != "" {
		glog.Fatal("--tls_disabled_route is provided as an option but tls_dir is empty. Please provide --tls_dir")
	}

	m := cmux.New(l)
	startServers(m)

	go func() {
		defer st.zero.closer.Done()
		err := m.Serve()
		if err != nil {
			glog.Errorf("error from cmux serve: %v", err)
		}
	}()
}

func startServers(m cmux.CMux) {
	httpRule := m.Match(func(r io.Reader) bool {
		//no tls config is provided. http is being used.
		if opts.tlsDir == "" {
			return true
		}
		//tls config is provided but none of the routes are disabled.
		if len(opts.tlsDisabledRoutes) == 0 {
			return false
		}
		path, ok := parseRequestPath(r)
		// not able to parse the request. Let it be resolved via TLS
		if !ok {
			return false
		}
		for _, r := range opts.tlsDisabledRoutes {
			if strings.HasPrefix(path, r) {
				return true
			}
		}
		return false
	})
	go startListen(httpRule)

	// if tls is enabled, make tls encryption based connections as default
	if Zero.Conf.GetString("tls_dir") != "" {
		tlsCfg, err := x.LoadServerTLSConfig(Zero.Conf, x.TLSNodeCert, x.TLSNodeKey)
		x.Check(err)
		if tlsCfg == nil {
			glog.Fatalf("tls_dir is set but tls config provided is not correct. Please define correct variable --tls_dir")
		}

		httpsRule := m.Match(cmux.Any())
		//this is chained listener. tls listener will decrypt the message and send it in plain text to HTTP server
		go startListen(tls.NewListener(httpsRule, tlsCfg))
	}
}

func startListen(l net.Listener) {
	srv := &http.Server{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  2 * time.Minute,
	}

	err := srv.Serve(l)
	glog.Errorf("Stopped taking more http(s) requests. Err: %v", err)
	ctx, cancel := context.WithTimeout(context.Background(), 630*time.Second)
	defer cancel()
	err = srv.Shutdown(ctx)
	glog.Infoln("All http(s) requests finished.")
	if err != nil {
		glog.Errorf("Http(s) shutdown err: %v", err)
	}
}

func parseRequestPath(r io.Reader) (path string, ok bool) {
	request, err := http.ReadRequest(bufio.NewReader(r))
	if err != nil {
		return "", false
	}

	return request.URL.Path, true
}