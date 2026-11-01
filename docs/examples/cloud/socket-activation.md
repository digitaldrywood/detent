# Retaining the ingress listener during a service restart

The Cloud entry and standalone Hub accept one systemd socket through
`LISTEN_PID` and `LISTEN_FDS`. Its TCP address must equal the configured listen
address. Without socket activation they bind their configured address directly.
An invalid activated descriptor fails startup rather than silently binding a
different listener.

The example `detent-cloud.socket` and `detent-cloud.service.d/socket.conf`
retain the production entry's loopback listener on port 8017. The corresponding
Hub examples retain port 7777. Adjust both the socket and the application's
configured address together for another environment. Keep the socket active
when restarting the service; do not attach its lifetime to the service with
`PartOf=`. The service requires the socket, but stopping the service does not
stop its socket.

Roll out socket activation in two steps. First, deploy a build that supports
socket activation through the existing CI receiver while the current direct-bind
units are still installed. The new build falls back to binding its configured
address when systemd does not pass an activated socket. Wait for that deployment
to pass its health check before changing the units. This step is required for
hosts running a binary that does not support `LISTEN_PID` and `LISTEN_FDS`.

Then install the units and service drop-ins from the repository with
`sudo ./deploy/hub/install-socket-activation.sh`. Perform the one-time listener
handoff for each service:

```sh
sudo systemctl stop detent-cloud.service
sudo systemctl enable --now detent-cloud.socket
sudo systemctl start detent-cloud.service

sudo systemctl stop detent-hub.service
sudo systemctl enable --now detent-hub.socket
sudo systemctl start detent-hub.service
```

The installer is repeatable and reloads systemd's unit definitions. The one-time
handoff must stop each directly bound service before starting its socket so the
socket can bind the configured port. Subsequent service restarts inherit that
socket. Merely deploying a binary does not install or activate the units.

The socket queues new connections while the service is stopped or opening its
databases. It does not preserve accepted connections, provide tenant readiness,
or authorize running two processes against the same database. Request and
ingress timeouts still apply. A successful transport handoff alone is not a
successful deployment health check.
