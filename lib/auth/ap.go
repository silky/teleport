/*
Copyright 2015 Gravitational, Inc.

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

package auth

import (
	"time"

	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"

	"github.com/codahale/lunk"
)

// AccessPoint is a interface needed by nodes to control the access
// to the node, and provide heartbeats
type AccessPoint interface {

	// GetLocalDomain returns domain name of the local authority server
	GetLocalDomain() (string, error)

	// GetServers returns a list of registered servers
	GetNodes() ([]services.Server, error)

	// UpsertServer registers server presence, permanently if ttl is 0 or
	// for the specified duration with second resolution if it's >= 1 second
	UpsertNode(s services.Server, ttl time.Duration) error

	// UpsertProxy registers server presence, permanently if ttl is 0 or
	// for the specified duration with second resolution if it's >= 1 second
	UpsertProxy(s services.Server, ttl time.Duration) error

	// GetProxies returns a list of proxy servers registered in the cluster
	GetProxies() ([]services.Server, error)

	// GetCertAuthorities returns a list of cert authorities
	GetCertAuthorities(caType services.CertAuthType, loadKeys bool) ([]*services.CertAuthority, error)

	// GetUsers returns a list of local users registered with this domain
	GetUsers() ([]services.User, error)

	// GetEvents returns a list of events that
	GetEvents(filter events.Filter) ([]lunk.Entry, error)
}
