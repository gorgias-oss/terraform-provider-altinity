// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// UserRequest is the body for creating/updating a cluster DB user
// (POST /cluster/{cluster}/users and POST /user/{id}). The wire shape mirrors
// reference.json's DbuserAdd/DbuserEditSql request schemas:
//
//   - databases is array<string>. Sending it as a plain string causes ACM's
//     PHP backend to mis-parse it and emit malformed ClickHouse SQL
//     ("Code: 62. DB::Exception: Syntax error"), so the provider always
//     splits configured CSV values into a real JSON array here.
//   - accessManagement is integer enum [0, 1]. The OpenAPI spec is explicit
//     that the field is NOT a JSON boolean. Pointer + omitempty lets the
//     Create path omit the field entirely when the configured value is the
//     default (false) so ACM doesn't emit a stray REVOKE on a freshly-
//     created user.
//   - networks is undocumented in the request schema but ACM accepts it as
//     array<string> (the same shape as its response field). Treat it the
//     same as databases.
type UserRequest struct {
	Login            string   `json:"login,omitempty"`
	Networks         []string `json:"networks,omitempty"`
	Databases        []string `json:"databases,omitempty"`
	AccessManagement *int     `json:"accessManagement,omitempty"`
	IDProfile        int64    `json:"id_profile,omitempty"`

	// Password is write-only; sent on create/update, never returned on Read.
	Password string `json:"password,omitempty"`
}

// FindUserByName locates a user by login within a cluster via ListUsers. Used
// by Create for idempotent adopt-by-login: an earlier failed Create may have
// left a half-created user in ACM (ACM commits the database row BEFORE
// executing the ClickHouse SQL, and a SQL error doesn't roll back the row),
// which would otherwise wedge subsequent retries.
func (c *Client) FindUserByName(ctx context.Context, clusterID int64, login string) (User, bool, error) {
	users, err := c.ListUsers(ctx, clusterID)
	if err != nil {
		return User{}, false, err
	}
	for i := range users {
		if users[i].Login == login {
			return users[i], true, nil
		}
	}
	return User{}, false, nil
}

// ListUsers reads all DB users for a cluster (GET /cluster/{cluster}/users).
// Read matches by Login (the config-stable key) and carries the ACM-internal
// id for subsequent update/delete (design §5.1).
func (c *Client) ListUsers(ctx context.Context, clusterID int64) ([]User, error) {
	var raw []wire.DbUser
	args := map[string]string{"cluster": clusterIDArg(clusterID)}
	if err := c.doJSON(ctx, wire.OpDbuserList, args, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]User, 0, len(raw))
	for i := range raw {
		u, err := userFromWire(&raw[i])
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// CreateUser adds a DB user to a cluster (POST /cluster/{cluster}/users) and
// returns the created user (with its assigned id).
func (c *Client) CreateUser(ctx context.Context, clusterID int64, req UserRequest) (User, error) {
	var w wire.DbUser
	args := map[string]string{"cluster": clusterIDArg(clusterID)}
	if err := c.doJSON(ctx, wire.OpDbuserAdd, args, req, &w); err != nil {
		return User{}, err
	}
	u, err := userFromWire(&w)
	if err != nil {
		return User{}, err
	}
	if u.ID == 0 {
		return User{}, fmt.Errorf("acm: DbuserAdd returned a user with id=0; the user may have been created — check the ACM UI before re-applying")
	}
	return u, nil
}

// UpdateUser updates a DB user cluster-scoped (POST /cluster/{cluster}/user/{id},
// operationId DbuserEditSql). Runs the generated SQL synchronously on the
// cluster and is gated by ACM's per-cluster "Cluster check" pre-flight — fine
// for regular user resources but rejects the admin user even on a healthy
// cluster (see UpdateUserGlobal).
//
// userRef is the {id} path segment. This endpoint is SQL-backed (it emits
// `ALTER USER '<name>'`), so the path is typed as a string and accepts EITHER
// ACM's numeric user id OR the login. The latter is essential: users ACM
// creates via DbuserAdd land in ClickHouse's `replicated` access storage with
// `hasModel: false` and NO numeric id (live-confirmed on cluster 10163, ACM
// build 25.8.x — only `users.xml` users like the bootstrap admin carry an id),
// so the login is the only handle the provider can address them by.
func (c *Client) UpdateUser(ctx context.Context, clusterID int64, userRef string, req UserRequest) (User, error) {
	var w wire.DbUser
	args := map[string]string{
		"cluster": clusterIDArg(clusterID),
		"id":      userRef,
	}
	if err := c.doJSON(ctx, wire.OpDbuserEditSql, args, req, &w); err != nil {
		return User{}, err
	}
	return userFromWire(&w)
}

// UpdateUserGlobal updates a DB user via the global, non-cluster-scoped
// endpoint (POST /user/{id}, operationId DbuserEdit). Unlike UpdateUser this
// path does NOT run SQL synchronously — it updates ACM's user record and the
// ACM operator autoPushes the change to the cluster out-of-band. The trade-off:
// no "Cluster check has failed" pre-flight (live-confirmed: the ACM UI itself
// uses this endpoint to rotate the admin password), at the cost of the password
// taking a beat longer to land in ClickHouse via the operator's reconcile loop.
//
// Use this for the cluster admin user, which the synchronous endpoint
// systematically rejects. Regular user resources stay on UpdateUser so an
// operator-driven edit is visible immediately.
func (c *Client) UpdateUserGlobal(ctx context.Context, userID int64, req UserRequest) (User, error) {
	var w wire.DbUser
	args := map[string]string{"id": strconv.FormatInt(userID, 10)}
	if err := c.doJSON(ctx, wire.OpDbuserEdit, args, req, &w); err != nil {
		return User{}, err
	}
	return userFromWire(&w)
}

// DeleteUser removes a DB user by its ACM-internal id, cluster-scoped
// (DELETE /cluster/{cluster}/user/{id}, operationId DbuserRemoveSql). The bare
// /user/{id} variant (DbuserRemove) returns HTTP 404 "Cluster not found" live,
// so the cluster id must be in the path — matching create/list/update. A 404 is
// surfaced as a *APIError so callers can treat already-deleted users as drift
// via IsNotFound.
// userRef is the {id} path segment: ACM's numeric user id when present, or the
// login for `replicated`-storage users that have no id (see UpdateUser).
func (c *Client) DeleteUser(ctx context.Context, clusterID int64, userRef string) error {
	args := map[string]string{
		"cluster": clusterIDArg(clusterID),
		"id":      userRef,
	}
	return c.doJSON(ctx, wire.OpDbuserRemoveSql, args, nil, nil)
}
