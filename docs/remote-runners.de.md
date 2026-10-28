# Entfernte Runner

Ein entfernter Runner ist ein Agent außerhalb des Clusters, etwa im Homelab-Netz, in einer
anderen Cloud oder auf einer VM, der Ressourcen eines Teams ausführt, die der Cluster nicht
erreicht. Er arbeitet wie ein selbst gehosteter GitHub-Actions-Runner: Er baut nur ausgehende
HTTPS-Verbindungen zum Runner-Endpunkt auf, holt sich eine Ausführung, führt OpenTofu lokal aus
und meldet das Ergebnis. Er führt nur das gepinnte Modul mit dem Spec-Snapshot aus, keine
beliebigen Schritte. Teams ohne Pool behalten Jobs im Cluster; beides läuft nebeneinander.

## Ablauf

1. Der Hook bindet eine neue Ressource eines Pool-Teams an den Pool, zusammen mit dem
   S3-State-Backend, dem Modul-Repository und dem Modulpfad (`status.runnerPool`,
   `status.moduleRepository`, `status.modulePath`).
2. Statt eines Jobs entsteht ein `Run` mit unveränderlichen Eingaben.
3. Ein Agent des Pools übernimmt ihn (`claim`), hält den Lease mit Heartbeats und meldet das
   Ergebnis mit einem idempotenten `commit`. Der Endpunkt schreibt Checkpoint,
   Connection-Secret und Graph an Ziele, die er aus dem Run ableitet, und setzt den Status.
4. Der Hook liest das Ergebnis wie bei einem Job: Phase, Fehlercode, Änderungszahlen, Drift.

Zugangsdaten bleiben am Agenten: Er liest sie je Team aus einem lokalen Profil. Der Claim
enthält keine Geheimnisse, und im `Run` stehen keine.

## Einrichtung

In `panko.json`:

```json
"runnerPools": [
  {"name": "edge", "teams": ["home"],
   "module": {"repository": "https://example.com/modules.git", "revision": "<commit>", "path": "modules/s3"}}
],
"runnerEndpoint": {"names": ["panko.example.com"]}
```

- `module` ist optional; ohne verwendet der Pool das Modul der Installation (`module.repository`). Der Modul-Mirror im Cluster
  ist für Agenten nicht erreichbar.
- `runnerEndpoint.names` sind die Namen oder IPs, unter denen Agenten den Endpunkt erreichen;
  das Zertifikat deckt sie ab. Im Lab ist der Endpunkt als NodePort 30443 erreichbar.

Dieselben Felder stehen in den Chart-Werten unter `installation` (ohne Checkout: `panko install
--values ...`). Dann:

```bash
make deploy
.local/bin/admin --kubeconfig .local/kubeconfig add-agent --pool edge   # druckt das Token einmal
.local/bin/admin --kubeconfig .local/kubeconfig endpoint-ca > ca.crt
```

Ressourcen eines Pool-Teams brauchen ein S3-State-Backend (`StateBackend` mit einem
`BackendProfile`), weil Agenten das Kubernetes-Backend im Team-Namespace nicht erreichen.

## Agent betreiben

Je Team ein Profil `profiles/<team>.env` mit Modus 0600 (für Kubernetes-Secret-Volumes 0440 über
`fsGroup`): S3-Zugangsdaten des State-Backends (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`) und
die Provider-Einstellungen des Moduls. Nur diese Werte erreichen OpenTofu; jede Ausführung bekommt
ein eigenes leeres `HOME`, gemeinsame AWS-Dateien sind abgeschaltet.

- Container: `docker build -t panko-agent:dev -f Dockerfile.agent .` (enthält `panko-server`
  mit der Rolle `agent`, OpenTofu und git), dann `panko-server agent run` mit
  `PANKO_ENDPOINT`, `PANKO_ENDPOINT_CA`, `PANKO_AGENT_TOKEN_FILE`, `PANKO_POOL`, `PANKO_PROFILES`.
- Systemdienst: `deploy/agent/panko-agent.service` mit `deploy/agent/agent.env.example`.
- Anderer Cluster: `deploy/agent/kubernetes.yaml`.

`panko-server agent check` prüft Werkzeuge, Profile, Arbeitsverzeichnis und ob der Endpunkt das Token für
diesen Pool annimmt; die systemd-Unit führt es vor dem Start aus. Modul- und Provider-Zugriff
zeigen sich erst je Run als Fehlercode. Optional hält `--plugin-cache` heruntergeladene Provider
zwischen Ausführungen; das ist sicher, weil ein Agent eine Ausführung nach der anderen bearbeitet.
Ein privates Modul-Repository braucht `GIT_SSH_COMMAND` mit Schlüssel und `known_hosts`.

## Tokens

- `admin add-agent --pool <pool>` registriert einen Agenten und druckt sein Token einmal; die
  Registry (`infra-runners/panko-runner-agents`) speichert nur Hashes.
- `admin rotate-agent --agent-id <id>` gibt ein neues Token aus; das alte gilt weiter bis
  `admin finish-rotation --agent-id <id>`.
- `admin revoke-agent --agent-id <id>` sperrt einen Agenten; alle Endpunkt-Replikas lehnen ihn
  nach höchstens zehn Sekunden ab, laufende Ausführungen enden mit ihrem Lease.

## Lease, Abbruch und Wiederherstellung

- Ein Lease läuft 60 Sekunden, Heartbeats alle 15 Sekunden. Der Agent beendet OpenTofu samt
  Prozessgruppe, bevor sein Lease ausläuft, wenn Heartbeats ausbleiben oder der Endpunkt
  abbricht; eine abgebrochene Ausführung meldet er nicht mehr.
- Je Pool ändern `leaseSeconds` und `heartbeatSeconds` diese Werte. Der Lease muss länger sein als
  zwei Heartbeat-Intervalle plus zehn Sekunden, damit vor dem Stopp von OpenTofu noch ein Heartbeat
  passt. Ein kürzerer Lease meldet einen verschwundenen Agenten früher als `interrupted`, verliert
  aber bei kurzen Netzausfällen schneller laufende Ausführungen.
- Läuft ein Lease ab, setzt der Endpunkt den Run auf `interrupted`. Wiederholt wird nur explizit
  (`panko retry`, Gateway-Aktion `retry`).
- Die S3-State-Sperre verhindert gleichzeitige OpenTofu-Läufe, schützt aber nicht vor einem
  abgeschnittenen Agenten, und ein getöteter Prozess kann sie zurücklassen. Panko löst sie nie
  automatisch. Wiederherstellung, nachdem sichergestellt ist, dass der alte Agent nicht mehr
  läuft: die Sperrdatei `<key>.tflock` im State-Bucket löschen (der Schlüssel steht in
  `status.stateBackend.config.key`) und den Run wiederholen. `test/remote_e2e.py` probt genau
  diesen Ablauf mit einem zweiten Agenten.
- Geht eine Claim-Antwort unterwegs verloren, wartet der Run bis zum Lease-Ablauf und wird dann
  `interrupted`.
- Ein Commit wird erst nach der Bestätigung verworfen. Bleibt ein Run in `Committing`, weil der
  Agent verschwand, markiert ihn der Endpunkt zehn Minuten nach der Frist als `interrupted`;
  alle Schreibvorgänge sind idempotent, ein Retry kommt zum selben Ergebnis.
- `admin pause` lässt den Endpunkt keine neuen Runs ausgeben und wartet auf übernommene.

## Zertifikate

Der Endpunkt erhält sein Zertifikat von einer eigenen stabilen CA (`runner-endpoint-tls-ca`) wie
der Provider-Mirror. Es wird 30 Tage vor Ablauf oder bei geänderten Namen aus derselben CA neu
ausgestellt, sobald der Setup-Job läuft (täglich per CronJob, sowie bei `make deploy` und
`panko install`), und vom laufenden Endpunkt ohne Neustart übernommen. Agenten behalten ihre CA
(`admin endpoint-ca`). Nur ein Austausch der CA selbst, angekündigt ein Jahr vor ihrem Ablauf,
verlangt, dass alle Agenten die neue CA erhalten. Nach dem Upgrade von einer Installation ohne
stabile CA liefert `admin endpoint-ca` ein Bündel aus bisheriger und neuer CA; Agenten sollten es
vor der nächsten Zertifikatserneuerung erhalten.

## Grenzen

- `migrate-backend` lehnt Ressourcen eines Pools ab.
- Pool-Wahl je Ressource (`spec.runnerPool`) gibt es noch nicht; ein Team hat höchstens einen Pool.
- Die Endpunkt-Rolle darf Secrets in Team-Namespaces schreiben; Kubernetes kann das Anlegen nicht
  auf Namen beschränken. Der Endpunkt leitet alle Ziele aus dem Run ab.
- Provider-Diagnosen können im Log des Agenten landen wie im Job-Log; ein Credential in einer
  Provider-Fehlermeldung ist dort sichtbar.
- Den Nachweis gegen eine emulierte Cloud liefert `test/remote_e2e.py` (Moto). Er zeigt das
  Zusammenspiel mit einem echten Cloud-Provider gegen eine emulierte API, nicht die
  Kompatibilität mit echtem AWS.
