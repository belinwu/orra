/*
 * This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at https://mozilla.org/MPL/2.0/.
 */

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/gilcrest/diygoapi/errs"
	"github.com/gorilla/mux"
	"github.com/olahol/melody"
	"github.com/rs/zerolog"
)

const JSONMarshalingFail = "Orra:JSONMarshalingFail"

type App struct {
	Plane  *ControlPlane
	Router *mux.Router
	Cfg    Config
	Logger zerolog.Logger
}

func NewApp(cfg Config, args []string) (*App, error) {
	lgr, err := NewLogger(args)
	if err != nil {
		return nil, err
	}

	return &App{
		Logger: lgr,
		Cfg:    cfg,
	}, nil
}

func (app *App) configureRoutes() *App {
	app.Router.HandleFunc("/register/project", app.RegisterProject).Methods(http.MethodPost)
	app.Router.HandleFunc("/apikeys", app.APIKeyMiddleware(app.CreateAdditionalApiKey)).Methods(http.MethodPost)
	app.Router.HandleFunc("/webhooks", app.APIKeyMiddleware(app.AddWebhook)).Methods(http.MethodPost)
	app.Router.HandleFunc("/register/service", app.APIKeyMiddleware(app.RegisterService)).Methods(http.MethodPost)
	app.Router.HandleFunc("/orchestrations", app.APIKeyMiddleware(app.OrchestrationsHandler)).Methods(http.MethodPost)
	app.Router.HandleFunc("/orchestrations", app.APIKeyMiddleware(app.ListOrchestrationsHandler)).Methods(http.MethodGet)
	app.Router.HandleFunc("/orchestrations/inspections/{id}", app.APIKeyMiddleware(app.OrchestrationInspectionHandler)).Methods(http.MethodGet)
	app.Router.HandleFunc("/register/agent", app.APIKeyMiddleware(app.RegisterAgent)).Methods(http.MethodPost)
	app.Router.HandleFunc("/ws", app.HandleWebSocket)
	return app
}

func (app *App) configureWebSocket() {
	app.Plane.WebSocketManager.melody.HandleConnect(func(s *melody.Session) {
		apiKey := s.Request.URL.Query().Get("apiKey")
		project, err := app.Plane.GetProjectByApiKey(apiKey)
		if err != nil {
			app.Logger.Error().Err(err).Msg("Invalid API key for WebSocket connection")
			return
		}
		svcID := s.Request.URL.Query().Get("serviceId")
		svcName, err := app.Plane.GetServiceName(project.ID, svcID)
		if err != nil {
			app.Logger.Error().Err(err).Msg("Unknown service for WebSocket connection")
			return
		}
		app.Plane.WebSocketManager.HandleConnection(svcID, svcName, s)
	})

	app.Plane.WebSocketManager.melody.HandleDisconnect(func(s *melody.Session) {
		serviceID, exists := s.Get("serviceID")
		if !exists {
			app.Logger.Error().Msg("serviceID missing from disconnected session")
			return
		}
		app.Plane.WebSocketManager.HandleDisconnection(serviceID.(string))
	})

	app.Plane.WebSocketManager.melody.HandleMessage(func(s *melody.Session, msg []byte) {
		app.Plane.WebSocketManager.HandleMessage(s, msg, func(serviceID string) (*ServiceInfo, error) {
			return app.Plane.GetServiceByID(serviceID)
		})
	})
}

func (app *App) Run() {
	port := app.Cfg.Port
	addr := fmt.Sprintf(":%d", port)

	srv := &http.Server{
		Addr: addr,
		// Good practice to set timeouts to avoid Slowloris attacks.
		// See: https://en.wikipediapp.org/wiki/Slowloris_(computer_security)
		WriteTimeout: time.Second * 180,
		ReadTimeout:  time.Second * 180,
		IdleTimeout:  time.Second * 180,
		Handler:      app.Router,
	}

	// Set up our server in s goroutine so that it doesn't block.
	go func() {
		app.Logger.Info().Msgf("Starting control plane on %s", addr)
		if err := srv.ListenAndServe(); err != nil {
			app.Logger.Info().Msg(err.Error())
		}
	}()

	c := make(chan os.Signal, 1)

	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal.
	<-c

	// Create s deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()
	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	_ = srv.Shutdown(ctx)

	app.Logger.Debug().Msg("http: All connections drained")
}

func (app *App) RegisterProject(w http.ResponseWriter, r *http.Request) {
	var project Project
	if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, errs.Code(JSONMarshalingFail), err))
		return
	}

	project.ID = app.Plane.GenerateProjectKey()
	project.APIKey = app.Plane.GenerateAPIKey()

	app.Plane.projects[project.ID] = &project

	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(project); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, errs.Code(JSONMarshalingFail), err))
		return
	}
}

func (app *App) RegisterServiceOrAgent(w http.ResponseWriter, r *http.Request, serviceType ServiceType) {
	apiKey := r.Context().Value("api_key").(string)
	project, err := app.Plane.GetProjectByApiKey(apiKey)
	if err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, err))
		return
	}

	var service ServiceInfo
	if err := json.NewDecoder(r.Body).Decode(&service); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, errs.Code(JSONMarshalingFail), err))
		return
	}

	service.ProjectID = project.ID
	service.Type = serviceType

	if err := app.Plane.RegisterOrUpdateService(&service); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}

	if err := json.NewEncoder(w).Encode(map[string]any{
		"id":     service.ID,
		"name":   service.Name,
		"status": Registered,
	}); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}
}

func (app *App) RegisterService(w http.ResponseWriter, r *http.Request) {
	app.RegisterServiceOrAgent(w, r, Service)
}

func (app *App) RegisterAgent(w http.ResponseWriter, r *http.Request) {
	app.RegisterServiceOrAgent(w, r, Agent)
}

func (app *App) OrchestrationsHandler(w http.ResponseWriter, r *http.Request) {
	apiKey := r.Context().Value("api_key").(string)
	project, err := app.Plane.GetProjectByApiKey(apiKey)
	if err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}

	var orchestration Orchestration
	if err := json.NewDecoder(r.Body).Decode(&orchestration); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, errs.Code(JSONMarshalingFail), err))
		return
	}

	app.Plane.PrepareOrchestration(project.ID, &orchestration)

	if !orchestration.Executable() {
		app.Logger.
			Debug().
			Str("Status", orchestration.Status.String()).
			Msgf("Orchestration %s cannot be executed: %s", orchestration.ID, orchestration.Error)

		w.WriteHeader(http.StatusUnprocessableEntity)
	} else {
		app.Logger.Debug().Msgf("About to execute orchestration %s", orchestration.ID)
		go app.Plane.ExecuteOrchestration(&orchestration)
		w.WriteHeader(http.StatusAccepted)
	}

	data, err := json.Marshal(orchestration)
	if err != nil {
		app.Logger.Error().Err(err).Interface("orchestration", orchestration).Msg("")
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, errs.Code(JSONMarshalingFail), err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(data); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}
}

func (app *App) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	serviceID := r.URL.Query().Get("serviceId")

	// Perform API key authentication
	apiKey := r.URL.Query().Get("apiKey")
	project, err := app.Plane.GetProjectByApiKey(apiKey)
	if err != nil {
		app.Logger.Error().Err(err).Msg("Invalid API key for WebSocket connection")
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unauthorized, err))
		return
	}

	if !app.Plane.ServiceBelongsToProject(serviceID, project.ID) {
		app.Logger.Error().Str("serviceID", serviceID).Msg("Service not found for the given project")
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unauthorized, err))
		return
	}

	if err := app.Plane.WebSocketManager.melody.HandleRequest(w, r); err != nil {
		app.Logger.Error().Str("serviceID", serviceID).Msg("Failed to handle request using the WebSocket")
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}
}

func (app *App) CreateAdditionalApiKey(w http.ResponseWriter, r *http.Request) {
	apiKey := r.Context().Value("api_key").(string)
	project, err := app.Plane.GetProjectByApiKey(apiKey)
	if err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, err))
		return
	}

	newApiKey := app.Plane.GenerateAPIKey()
	project.AdditionalAPIKeys = append(project.AdditionalAPIKeys, newApiKey)

	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"apiKey": newApiKey,
	}); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}
}

func (app *App) AddWebhook(w http.ResponseWriter, r *http.Request) {
	apiKey := r.Context().Value("api_key").(string)
	project, err := app.Plane.GetProjectByApiKey(apiKey)
	if err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, err))
		return
	}

	var webhook struct {
		Url string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&webhook); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, errs.Code(JSONMarshalingFail), err))
		return
	}

	if _, err := url.ParseRequestURI(webhook.Url); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Validation, err))
		return
	}

	project.Webhooks = append(project.Webhooks, webhook.Url)

	// Return the new key
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(webhook); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}
}

func (app *App) ListOrchestrationsHandler(w http.ResponseWriter, r *http.Request) {
	apiKey := r.Context().Value("api_key").(string)
	project, err := app.Plane.GetProjectByApiKey(apiKey)
	if err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, err))
		return
	}

	orchestrationList := app.Plane.GetOrchestrationList(project.ID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(orchestrationList); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Internal, err))
		return
	}
}

func (app *App) OrchestrationInspectionHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	apiKey := r.Context().Value("api_key").(string)
	project, err := app.Plane.GetProjectByApiKey(apiKey)
	if err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.InvalidRequest, err))
		return
	}

	orchestrationID := vars["id"]

	if !app.Plane.OrchestrationBelongsToProject(orchestrationID, project.ID) {
		err := fmt.Errorf("unknown orchestration")
		app.Logger.
			Error().
			Str("ProjectID", project.ID).
			Str("OrchestrationID", orchestrationID).
			Msg("Orchestration not found for the given project")
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unauthorized, err))
		return
	}

	inspection, err := app.Plane.InspectOrchestration(orchestrationID)
	if err != nil {
		app.Logger.
			Error().
			Err(err).
			Str("OrchestrationID", orchestrationID).
			Msg("Failed to inspect orchestration")
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Unanticipated, err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(inspection); err != nil {
		errs.HTTPErrorResponse(w, app.Logger, errs.E(errs.Internal, err))
		return
	}
}
