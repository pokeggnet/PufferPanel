/*
 Copyright 2018 Padduck, LLC
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at
 	http://www.apache.org/licenses/LICENSE-2.0
 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package api

import (
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/pufferpanel/apufferi"
	"github.com/pufferpanel/apufferi/logging"
	builder "github.com/pufferpanel/apufferi/response"
	"github.com/pufferpanel/pufferpanel"
	"github.com/pufferpanel/pufferpanel/database"
	"github.com/pufferpanel/pufferpanel/models"
	"github.com/pufferpanel/pufferpanel/services"
	"github.com/pufferpanel/pufferpanel/web/handlers"
	"github.com/satori/go.uuid"
	"io"
	"net/http"
	"reflect"
	"strconv"
)

func registerServers(g *gin.RouterGroup) {
	g.Handle("GET", "", handlers.OAuth2WithLimit(pufferpanel.ScopeViewServers, false), searchServers)
	g.Handle("OPTIONS", "", pufferpanel.CreateOptions("GET"))

	g.Handle("POST", "", handlers.OAuth2(pufferpanel.ScopeCreateServers, false), createServer)
	g.Handle("GET", "/:serverId", handlers.OAuth2(pufferpanel.ScopeViewServers, true), getServer)
	g.Handle("PUT", "/:serverId", handlers.OAuth2(pufferpanel.ScopeEditServers, false), createServer)
	g.Handle("POST", "/:serverId", handlers.OAuth2(pufferpanel.ScopeEditServerAsUser, true), createServer)
	g.Handle("DELETE", "/:serverId", handlers.OAuth2(pufferpanel.ScopeEditServers, false), deleteServer)
	g.Handle("GET", "/:serverId/user", handlers.OAuth2(pufferpanel.ScopeEditServers, true), getServerUsers)
	g.Handle("GET", "/:serverId/user/:username", handlers.OAuth2(pufferpanel.ScopeEditServerUsers, true), getServerUsers)
	g.Handle("PUT", "/:serverId/user/:username", handlers.OAuth2(pufferpanel.ScopeEditServerUsers, true), editServerUser)
	g.Handle("DELETE", "/:serverId/user/:username", handlers.OAuth2(pufferpanel.ScopeEditServerUsers, true), removeServerUser)
	g.Handle("OPTIONS", "/:serverId", pufferpanel.CreateOptions("PUT", "GET", "POST", "DELETE"))
}

func searchServers(c *gin.Context) {
	var err error
	response := builder.From(c)

	username := c.DefaultQuery("username", "")
	nodeQuery := c.DefaultQuery("node", "0")
	nameFilter := c.DefaultQuery("name", "*")
	pageSizeQuery := c.DefaultQuery("limit", strconv.Itoa(DefaultPageSize))
	pageQuery := c.DefaultQuery("page", strconv.Itoa(1))

	pageSize, err := strconv.Atoi(pageSizeQuery)
	if err != nil || pageSize <= 0 {
		response.Fail().Status(http.StatusBadRequest).Message("page size must be a positive number")
		return
	}

	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}

	page, err := strconv.Atoi(pageQuery)
	if err != nil || page <= 0 {
		response.Fail().Status(http.StatusBadRequest).Message("page must be a positive number")
		return
	}

	node, err := strconv.Atoi(nodeQuery)
	if err != nil || page <= 0 {
		response.Fail().Status(http.StatusBadRequest).Message("node id is invalid")
		return
	}

	db, err := database.GetConnection()
	if pufferpanel.HandleError(response, err) {
		return
	}

	ss := &services.Server{DB: db}
	os := services.GetOAuth(db)

	//see if user has access to view all others, otherwise we can't permit search without their username
	ci, allowed, _ := os.HasRights(c.GetString("accessToken"), nil, pufferpanel.ScopeViewServers)
	if !allowed {
		response.PageInfo(uint(page), uint(pageSize), MaxPageSize, 0).Data(make([]models.ServerView, 0))
		return
	}

	username = ci.User.Username

	var results *models.Servers
	var total uint
	searchCriteria := services.ServerSearch{
		Username: username,
		NodeId:   uint(node),
		Name:     nameFilter,
		PageSize: uint(pageSize),
		Page:     uint(page),
	}
	if results, total, err = ss.Search(searchCriteria); pufferpanel.HandleError(response, err) {
		return
	}

	response.PageInfo(uint(page), uint(pageSize), MaxPageSize, total).Data(models.RemoveServerPrivateInfoFromAll(models.FromServers(results)))
}

func getServer(c *gin.Context) {
	response := builder.From(c)

	t, exist := c.Get("server")

	if !exist {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	server, ok := t.(*models.Server)
	if !ok {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
	}

	response.Data(models.RemoveServerPrivateInfo(models.FromServer(server)))
}

func createServer(c *gin.Context) {
	var err error
	response := builder.From(c)

	serverId := c.Param("id")
	if serverId == "" {
		serverId = uuid.NewV4().String()[:8]
	}

	postBody := &serverCreation{}
	err = c.Bind(postBody)
	postBody.Identifier = serverId
	if err != nil {
		response.Status(http.StatusBadRequest).Error(err).Fail()
		return
	}

	db, err := database.GetConnection()
	if pufferpanel.HandleError(response, err) {
		return
	}

	//time for a transaction!
	trans := db.Begin()
	success := false
	defer func() {
		if !success {
			trans.Rollback()
		}
	}()

	ss := &services.Server{DB: trans}
	ns := &services.Node{DB: trans}
	os := services.GetOAuth(trans)
	us := &services.User{DB: trans}

	node, exists, err := ns.Get(postBody.NodeId)

	if pufferpanel.HandleError(response, err) {
		return
	}

	if !exists {
		response.Status(http.StatusBadRequest).Message("no node with given id").Fail()
	}

	server := &models.Server{
		Name:       getFromDataOrDefault(postBody.Variables, "name", postBody.Identifier).(string),
		Identifier: postBody.Identifier,
		NodeID:     node.ID,
		IP:         getFromDataOrDefault(postBody.Variables, "ip", "0.0.0.0").(string),
		Port:       getFromDataOrDefault(postBody.Variables, "port", uint(0)).(uint),
		Type:       postBody.Type,
	}

	users := make([]*models.User, len(postBody.Users))

	for k, v := range postBody.Users {
		user, exists, err := us.Get(v)
		if pufferpanel.HandleError(response, err) {
			return
		}
		if !exists {
			pufferpanel.HandleError(response, pufferpanel.ErrUserNotFound.Metadata(map[string]interface{}{"username": v}))
			return
		}

		users[k] = user
	}

	admins, err := os.GetByScope(pufferpanel.ScopeServerAdmin, nil, nil, true)
	if pufferpanel.HandleError(response, err) {
		return
	}
	for _, v := range *admins {
		users = append(users, &v.User)
	}

	err = ss.Create(server)
	if err != nil {
		response.Status(http.StatusInternalServerError).Error(err).Fail()
		return
	}

	for _, v := range users {
		_, err := os.Create(v, server, "", true, pufferpanel.GetDefaultUserServerScopes()...)
		if pufferpanel.HandleError(response, err) {
			return
		}
	}

	data, _ := json.Marshal(postBody.Server)
	reader := newFakeReader(data)

	headers := http.Header{}
	headers.Set("Authorization", c.GetHeader("Authorization"))

	nodeResponse, err := ns.CallNode(node, "PUT", "/server/"+server.Identifier, reader, headers)

	if pufferpanel.HandleError(response, err) {
		return
	}

	if nodeResponse.StatusCode != http.StatusOK {
		logging.Build(logging.ERROR).WithMessage("Unexpected response from daemon: %+v").WithArgs(nodeResponse.StatusCode).Log()
		pufferpanel.HandleError(response, pufferpanel.ErrUnknownError)
		return
	}

	apiResponse := &builder.Response{}
	err = json.NewDecoder(nodeResponse.Body).Decode(apiResponse)

	if pufferpanel.HandleError(response, err) {
		return
	}

	if !apiResponse.Success {
		logging.Build(logging.ERROR).WithMessage("Unexpected response from daemon: %+v").WithArgs(apiResponse).Log()
		pufferpanel.HandleError(response, pufferpanel.ErrUnknownError)
		return
	}

	response.Data(server.Identifier)

	trans.Commit()
	success = true
}

func deleteServer(c *gin.Context) {
	var err error
	response := builder.From(c)

	db, err := database.GetConnection()
	if pufferpanel.HandleError(response, err) {
		return
	}

	ss := &services.Server{DB: db}

	t, exist := c.Get("server")

	if !exist {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	server, ok := t.(*models.Server)
	if !ok {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	err = ss.Delete(server.ID)
	if pufferpanel.HandleError(response, err) {
		return
	} else {
		v := models.FromServer(server)
		response.Status(http.StatusOK).Data(v)
	}
}

func getServerUsers(c *gin.Context) {
	var err error
	response := builder.From(c)

	db, err := database.GetConnection()
	if pufferpanel.HandleError(response, err) {
		return
	}

	os := services.GetOAuth(db)

	t, exist := c.Get("server")

	if !exist {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	server, ok := t.(*models.Server)
	if !ok {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	clients, err := os.GetForServer(server.ID, false)
	if pufferpanel.HandleError(response, err) {
		return
	}

	users := make([]userScopes, 0)
	for _, client := range *clients {
		scopes := make([]string, 0)

		for _, scope := range client.ServerScopes {
			scopes = append(scopes, scope.Scope)
		}

		users = append(users, userScopes{
			Username: client.User.Username,
			Scopes:   scopes,
		})
	}

	response.Data(users)
}

func editServerUser(c *gin.Context) {
	var err error
	response := builder.From(c)

	username := c.Param("username")
	if username == "" {
		return
	}

	replacement := &userScopes{}
	err = c.BindJSON(replacement)
	if pufferpanel.HandleError(response, err) {
		return
	}
	replacement.Username = username

	db, err := database.GetConnection()
	if pufferpanel.HandleError(response, err) {
		return
	}

	us := &services.User{DB: db}
	os := services.GetOAuth(db)

	t, exist := c.Get("server")

	if !exist {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	server, ok := t.(*models.Server)
	if !ok {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	user, exists, err := us.Get(username)
	if !exists || pufferpanel.HandleError(response, err) {
		return
	}

	clientId := services.CreateInternalClientId(user, server)
	client, exists, err := os.GetByClientId(clientId)
	if pufferpanel.HandleError(response, err) {
		return
	}
	if !exist {
		_, err = os.Create(user, server, clientId, true, replacement.Scopes...)
	} else {
		err = os.UpdateScopes(client, server, replacement.Scopes...)
	}
	
	pufferpanel.HandleError(response, err)
}

func removeServerUser(c *gin.Context) {
	var err error
	response := builder.From(c)

	username := c.Param("username")
	if username == "" {
		return
	}

	db, err := database.GetConnection()
	if pufferpanel.HandleError(response, err) {
		return
	}

	us := &services.User{DB: db}
	os := services.GetOAuth(db)

	t, exist := c.Get("server")

	if !exist {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	server, ok := t.(*models.Server)
	if !ok {
		pufferpanel.HandleError(response, pufferpanel.ErrServerNotFound)
		return
	}

	user, exists, err := us.Get(username)
	if !exists || pufferpanel.HandleError(response, err) {
		return
	}

	err = os.Delete(services.CreateInternalClientId(user, server))

	pufferpanel.HandleError(response, err)
}

//This class exists
type fakeReader struct {
	reader io.Reader
}

func newFakeReader(data []byte) *fakeReader {
	return &fakeReader{reader: bytes.NewReader(data)}
}

func (fr *fakeReader) Read(p []byte) (int, error) {
	return fr.reader.Read(p)
}

func (fr *fakeReader) Close() error {
	return nil
}

type serverCreation struct {
	apufferi.Server

	NodeId uint     `json:"node"`
	Users  []string `json:"users"`
}

func getFromData(variables map[string]apufferi.Variable, key string) interface{} {
	for k, v := range variables {
		if k == key {
			return v.Value
		}
	}
	return nil
}

//this will enforce whatever the type val is defined as will be what is returned
func getFromDataOrDefault(variables map[string]apufferi.Variable, key string, val interface{}) interface{} {
	res := getFromData(variables, key)

	if res != nil {
		if reflect.TypeOf(val).AssignableTo(reflect.TypeOf(res)) {
			return res
		}
	}

	return val
}

type userScopes struct {
	Username string   `json:"username,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}
