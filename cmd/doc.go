// Package main is the entry point for the notification service.

// @title			notifications API
// @version			1.0
// @description		Notification service of the DHBW Cloud self-service platform. Services post events (POST /v1/events, service token); the service decides whether, when and how they reach people. People read and change their settings through the portal (ID token).

// @securityDefinitions.apikey	Bearer
// @in							header
// @name						Authorization
// @description					Bearer token — value: "Bearer <token>": a service token for /v1/events, the portal's ID token for the person routes, an admin token for /v1/admin.
package main
