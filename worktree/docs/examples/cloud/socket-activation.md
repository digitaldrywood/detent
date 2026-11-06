# Keeping the ingress listener during restarts

Cloud entry and standalone Hub support systemd socket activation through
`LISTEN_PID` and `LISTEN_FDS`. The socket address must match the address in the
application configuration. Without an activated socket, the application binds
its configured address directly. An invalid activated descriptor fails
startup instead of silently binding a different listener.

Release deployments use the restricted CI receiver. On the first deployment
where the selected Cloud or standalone Hub socket is not active, the receiver
renders and installs only that service's socket unit and service drop-in, then
enables the socket and starts the service with the candidate binary. It does
not start the unused standalone Hub while deploying Cloud entry. Before
installation, the receiver requires the candidate's version JSON to declare
`socket_activation_supported: true`; older or unknown binaries are rejected
before the service is stopped. The production
Cloud service and socket are `detent-cloud.service` and `detent-cloud.socket`
on `127.0.0.1:8017`. Staging uses `detent-cloud-staging.service` and
`detent-cloud-staging.socket` on `127.0.0.1:8018`. Standalone Hub uses
`detent-hub.service` or `detent-hub-staging.service` and listens on
`127.0.0.1:7777`.

The first direct-bind handoff cannot guarantee zero connection refusals: the
existing service must stop before systemd can bind the same address. This
one-time bootstrap is performed by the authorized release receiver and can
briefly refuse new connections. The socket remains active afterward. Later
deployments restart only the selected service; the socket keeps accepting and
queues connections while the process opens its databases and waits for ready
tenant Hubs. The receiver reports deployment success only after the candidate
process and complete schema health facts are ready. A transport handoff alone
does not make a deployment healthy.

If the first candidate fails health and the receiver's schema compatibility
check permits rollback, it stops the candidate, disables the new socket,
removes the service drop-in and socket unit, and restores the previous
direct-binding service. If the schema evidence is unknown or incompatible, the
candidate remains selected for recovery rather than starting a binary that may
not open the migrated databases.

The example `detent-cloud.socket` and
`detent-cloud.service.d/socket.conf` retain the production Cloud listener. The
corresponding Hub examples retain the standalone Hub listener. For a local
systemd deployment outside the restricted release path, install these examples
with `deploy/hub/install-socket-activation.sh`; update the socket and
application addresses together when using a different port.

The socket does not preserve already accepted connections, bypass request
timeouts, or authorize two processes to use the same database. Keep it active
when restarting the service, and do not bind another service to its address.
