# Betrieb, Migration und Diagnose

## Tests und CI

Die Go-Unit- und Vertragstests benötigen Go aus `mise.toml`, aber weder Docker noch
einen Kubernetes-Cluster. Für Python-Linting und externe E2E-Tests zusätzlich die
projektlokale Python-Umgebung vorbereiten:

```sh
mise exec -- python3 -m venv .venv
.venv/bin/python -m pip install -r requirements-dev.txt
make test-unit lint-go lint
```

Das OpenTofu-Modul liegt im Repository `hingst-it/panko-database` und wird dort
formatiert und validiert. `make test` ergänzt die effektiven
RBAC-Prüfungen, PostgreSQL-Lebenszyklen, Gateway-/OpenAPI-Tests sowie externe
Runner- und Migrationstests im lokalen Cluster. Voraussetzung sind `make cluster-up bootstrap build deploy` und die
installierten Testabhängigkeiten. `PYTHON=/pfad/zum/python` überschreibt den
Standard `.venv/bin/python` für Python-Linting. Unit-Tests laufen mit Go und Race Detector.

`.github/workflows/ci.yaml` führt bei Push und Pull Request zuerst Unit-Tests,
und Linting aus. Ein separater Job erstellt danach einen k3d-Cluster und führt
`make test` aus. Er liest den Deploy-Key des Modul-Repositorys aus dem
Actions-Secret `PANKO_MODULE_DEPLOY_KEY`. Fehlerdiagnosen enthalten Ressourcenstatus,
Events und Dienstlogs; Secrets und States werden nicht als CI-Artefakte exportiert.
Der Testcluster wird auch bei Fehlern entfernt.

Go-Aufbau, gesicherte Python-Images und geprüfter Rückwechsel: [Go-Portierung](go-porting.de.md).
Team-definierte State-Backends, Sicherungen und Migration: [State-Backends](state-backends.de.md).
Ein bestehendes Lab braucht für den S3-Gateway dieser Funktion einmalig `make bootstrap deploy`.

## Teams und Konfiguration

`panko.json` im Repository legt fest, welche Teams es gibt und ob das Lab-S3 läuft:

```json
{
  "teams": ["team-a", "team-b"],
  "s3": true
}
```

Teamnamen sind DNS-Labels mit höchstens 40 Zeichen; Präfixe wie `infra-`, `kube-` und
`panko-` sind reserviert. Alle Generatoren lesen diese Datei: Namespaces, RBAC,
Gateway-Tokens, Modul-Schlüssel, S3-Benutzer und Profile. Ein Team kommt hinzu mit

```sh
.local/bin/admin add-team --team home
make deploy
```

`make deploy` ergänzt fehlende Team-Tokens im Secret `gateway-auth` und in
`.local/team-tokens.json`; vorhandene Tokens bleiben unverändert. Geht die lokale Datei
verloren, stellt `make deploy` sie aus dem Secret wieder her. Teams zu entfernen wird
nicht unterstützt; ihre Ressourcen und States müssten vorher gelöscht werden.

Mit `"s3": false` installieren `make bootstrap` und `make deploy` weder den S3-Gateway
noch Team-Profile; Ressourcen verwenden dann das Kubernetes-Backend.

### Upgrade

Ein Upgrade von Panko selbst: neuen Stand auschecken, dann `make build deploy`. Images
werden neu gebaut und ausgerollt; bestehende Ressourcen behalten Bindung, State und
Status und werden nicht erneut angewendet. Nur eine geänderte `MODULE_REVISION` wendet
jede Ressource einmal mit dem neuen Modul an. Neue Lab-Bestandteile wie der S3-Gateway
erfordern zusätzlich `make bootstrap`.

## Egress-Regeln für Runner

Mit `"egress": true` in `panko.json` erzeugt `make deploy` je Team-Namespace die
NetworkPolicy `panko-runner-egress`. Sie gilt nur für Runner-Pods und erlaubt
ausschließlich DNS, die Kubernetes-API, Provider- und Modul-Mirror, das Lab-S3 und die
Ziele aus `egressTargets`:

```json
"egressTargets": [
  {"service": "infra-backends/postgres", "port": 5432},
  {"cidr": "192.168.1.10/32", "port": 3306}
]
```

Dienste werden auf ihre Pods und den Ziel-Port aufgelöst, weil Policies Pod-Adressen
sehen; externe Ziele werden als CIDR angegeben. Hostnamen sind mit Standard-Policies
nicht möglich. Mit aktiven Regeln entfällt der direkte Rückfall auf die
Provider-Registry: Ist der Provider-Mirror ausgefallen, scheitern Jobs. Deshalb ist die
Einstellung standardmäßig aus. `"egress": false` entfernt die Policies beim nächsten
Deployment wieder. `test/egress_e2e.py` prüft beide Seiten: Provisionierung und
deklarierte Ziele funktionieren, Gateway und Internet sind gesperrt.

Policy-Controller wie kube-router (k3s/k3d) installieren die Regeln eines neuen Pods
asynchron, typischerweise wenige Sekunden nach dem Start. In diesem Fenster ist ein
Runner-Pod noch unbeschränkt; die Regeln sind eine Härtung, keine Garantie ab der
ersten Sekunde.

## Drift-Prüfung und Modul-Rollouts

`panko plan NAME --wait` (oder `POST .../{name}/plan`) startet für eine Ressource im
Zustand `Ready` eine lesende Prüfung: `init`, Backend- und Checkpoint-Prüfung, dann
`tofu plan` ohne Speichern. Das Ergebnis steht in `status.lastPlan` mit `drift` und
den Änderungszahlen; die Phase bleibt `Ready`, nichts wird repariert. Exit-Code 3
meldet Drift. Mit `DRIFT_CHECK_INTERVAL` am Hook (mindestens `10m`, z. B. `6h`)
prüft Panko jede Ressource in diesem Abstand selbst, höchstens einmal je Intervall.

Standardmäßig (`MODULE_ROLLOUT=auto`) wendet eine neue `MODULE_REVISION` jede
Ressource einmal an. Mit `MODULE_ROLLOUT=manual` bleiben bestehende Ressourcen auf
ihrer angewendeten Revision; nur neue erhalten die konfigurierte. Umgestellt wird
gezielt:

```sh
.local/bin/admin upgrade-module --namespace team-a --name orders   # Kanarienvogel
.local/bin/admin upgrade-module --all                              # danach alle
```

Destroy verwendet weiterhin die zuletzt angewendete Revision.

## Sicherung und Wiederherstellung

```sh
make backup BACKUP_DIR=.local/backup-20261024
make restore BACKUP_DIR=.local/backup-20261024   # in eine frisch installierte Umgebung
```

`make backup` stoppt Hook und Gateway, wartet, bis kein Runner-Job mehr läuft, und schreibt
in ein neues, privates Verzeichnis:

| Datei | Inhalt |
| --- | --- |
| `cluster.json` | Databases mit Spec, Status und Identität, StateBackends, BackendProfiles, State- und Checkpoint-Secrets, Zugangsdaten (PostgreSQL-Admin, S3, Gateway-Tokens) und die Installations-ID |
| `postgres.sql` | `pg_dumpall` aller Datenbanken und Rollen |
| `s3.tar` | Daten und Benutzer des Lab-S3, falls aktiviert |

Danach laufen Hook und Gateway wieder an. Das Verzeichnis enthält Zugangsdaten und
Datenbankinhalte und gehört nicht ins Repository oder auf geteilte Ablagen.

`make restore` erwartet eine frische Installation (`make cluster-up bootstrap build deploy`)
und prüft vor dem ersten Schreibzugriff, dass keine fremden Databases existieren. Es stoppt Hook und Gateway,
spielt Zugangsdaten, Installations-ID, States, Checkpoints und Databases ein, stellt
Datenbankinhalte und S3-Daten wieder her und gleicht `.local/team-tokens.json` mit dem
wiederhergestellten Secret ab. Neu angelegte Databases erhalten von Kubernetes eine neue
UID; `status.identity` behält die ursprüngliche, sodass Namen, State-Schlüssel und
Checkpoints unverändert bleiben. Nach dem Start wendet jede Database einmal gegen ihren
wiederhergestellten State an, ohne Datenbanken oder Rollen neu anzulegen; dabei entstehen
auch die Connection-Secrets neu. SQL-Fehler brechen die Wiederherstellung ab, die Ausgabe
von psql liegt dann in `restore-psql.log` im Sicherungsverzeichnis. Schlägt ein Schritt
fehl, bleibt die Installation pausiert; ein erneutes `make restore` mit demselben
Verzeichnis setzt fort.

`test/restore_rehearsal.sh` probt den Ablauf vollständig: Lab mit Daten anlegen,
sichern, löschen, neu aufbauen, wiederherstellen, Daten und Zugangsdaten prüfen,
ändern und löschen.

## Engine und Modul

Runner-Jobs setzen sich beim Start aus drei Teilen zusammen. Der Init-Container
`engine` kopiert das OpenTofu-Binary aus `ENGINE_IMAGE` in ein gemeinsames Volume.
Der Init-Container `module` holt das Modul per git-sync über `MODULE_MIRROR`
(`git://module-mirror.infra-runners.svc:9418/module.git`) zum Commit
`MODULE_REVISION`, ohne Zugangsdaten. Danach führt der Runner das kopierte Binary gegen dieses
Modul aus.
Beide Images sind per Digest festgelegt, das Modul per vollständigem Commit-SHA; der
Hook startet nicht, wenn die Revision ein Branch oder Tag ist.

Provider lädt OpenTofu beim Start über den unten beschriebenen Mirror oder direkt aus der Registry. `-lockfile=readonly` prüft sie
gegen das Lockfile des Moduls, das deshalb Prüfsummen aller Runner-Plattformen
enthalten muss. Jobs benötigen Zugriff auf den Modul-Mirror und den
Provider-Mirror beziehungsweise die Provider-Registry. Nur der Modul-Mirror
benötigt für das Modul Zugriff auf GitHub.

Ein Modul-Update bedeutet: neuen Commit-SHA als `MODULE_REVISION` in
`controller/deployment.yaml` eintragen und `make deploy` ausführen. Der Commit
muss auf einem Branch des in `cluster/module-mirror.yaml` konfigurierten
`MODULE_REPO` liegen, den der Mirror abholt. Dieser klont beim Start einmal
mit `git clone --mirror` und aktualisiert danach alle 60 Sekunden mit
`git remote update --prune`. Vor einem Rollout muss der neue Commit im Mirror
verfügbar sein; `make deploy` startet ihn neu und wartet auf seine Bereitschaft.
Jede Database wird danach einmal mit der neuen Revision angewendet. Der Status hält mit
`lastAppliedRevision` fest, welche Revision zuletzt erfolgreich angewendet wurde;
Destroy verwendet genau diese Revision. Ressourcen ohne erfasste Revision werden
beim ersten Deployment dieses Aufbaus einmal angewendet.

Der Deploy-Key ist nur lesend für das Modul-Repository freigegeben. `make deploy`
legt ihn mit `admin module-mirror-secret` als Secret `panko-module-git`
ausschließlich in `infra-runners` ab, zusammen mit den Host-Schlüsseln aus
`cluster/module-known-hosts`. Diese Host-Schlüssel stammen aus GitHubs API
(`gh api meta`) und müssen bei einem Schlüsselwechsel von GitHub aktualisiert
werden. Nur der Modul-Mirror mountet dieses Secret; SSH
prüft die Host-Schlüssel strikt. Nach dem Hook-Rollout löscht das Deployment
alte Kopien des Secrets aus den konfigurierten Team-Namespaces.
Der Mirror stellt das Bare-Repository im Cluster über einen nur lesenden,
unauthentifizierten Git-Dienst auf Port 9418 bereit; Push ist deaktiviert.
Damit kann jeder Pod im Cluster den Modulcode lesen, aber keinen Schlüssel. Für eine
persönliche Installation ist das bewusst akzeptiert; mit Egress-Regeln (Roadmap 3c)
lässt sich der Zugriff auf Runner-Pods beschränken.

Ist der Modul-Mirror ausgefallen oder fehlt die angeforderte Revision, scheitert
der `module`-Init-Container; der Status meldet `module_fetch`. Es gibt keinen
Fallback auf GitHub und keinen Deploy-Key im Job. Ein fehlgeschlagenes
periodisches Update lässt den zuletzt geholten Stand weiter verfügbar. Das
Repository liegt in einem `emptyDir` und wird nach einem Pod-Neustart neu
geklont. Auch Admin-Migrationsjobs übernehmen `MODULE_MIRROR` aus der
Hook-Konfiguration. Ohne `MODULE_MIRROR` bleibt der bisherige SSH-Modus mit
`MODULE_REPO` und Team-Secrets (`admin module-secret`) verfügbar.

Ein separates Lab lässt sich mit `make CLUSTER_NAME=infra-lab-go KUBECONFIG="$PWD/.local/go-fresh-kubeconfig" cluster-up bootstrap build deploy` erstellen. Für alle weiteren
Make-Aufrufe dieselben Werte verwenden. `make cluster-down` entfernt ausschließlich
den ausgewählten Cluster.

## Provider-Mirror

`make build deploy` baut und startet `provider-mirror` in `infra-runners` unter
`https://provider-mirror.infra-runners.svc:8443`. `admin ensure-mirror-tls` erzeugt
nur bei fehlendem Secret `provider-mirror-tls` eine ECDSA-P-256-CA (fünf Jahre)
und ein Serverzertifikat (ein Jahr) für beide Service-DNS-Namen. Der CA-Schlüssel
wird nach dem Signieren verworfen; gespeichert wird nur der Server-Schlüssel.
`admin --kubeconfig "$KUBECONFIG" mirror-ca | kubectl apply -f -` verteilt allein
das CA-Zertifikat als ConfigMap `panko-mirror-ca` an die Team-Namespaces.

`PROVIDER_MIRROR` am Hook aktiviert die optionale CA-Einbindung in neue Jobs.
Der Runner prüft vor `init` (auch bei Migrationen) `/healthz` mit zwei Sekunden
Timeout und Systemwurzeln plus `/mirror-ca`. Bei Erfolg schreibt er eine private
CLI-Konfiguration mit `network_mirror` und setzt `TF_CLI_CONFIG_FILE` sowie
`SSL_CERT_DIR=/etc/ssl/certs:/mirror-ca`; `SSL_CERT_FILE` wird dafür nicht gesetzt.
Bei fehlender CA oder unerreichbarem Dienst verwendet er direkten Registry-Zugriff
und protokolliert einmal `provider_mirror_unavailable` mit `level=warning`.
OpenTofu selbst kann weder HTTP-Mirrors nutzen noch bei einem Mirror-Ausfall auf
`direct` zurückfallen. Ein Ausfall nach erfolgreicher Probe kann daher `init`
scheitern lassen. Ohne `PROVIDER_MIRROR` bleibt der Job-Vertrag unverändert.

Der Mirror erlaubt standardmäßig `registry.opentofu.org`, `registry.terraform.io`
und `linux_amd64`, `linux_arm64`. Flags `-hosts`, `-platforms` und `-index-ttl`
ändern diese Auswahl; eine leere Plattformliste erlaubt alle Plattformen.
Indizes gelten fünf Minuten (höchstens eine Stunde), Versionsmetadaten und ZIPs
bleiben unverändert. Nur vollständig heruntergeladene, gegen `shasum` geprüfte
ZIPs gelangen atomar in den Cache. Das Modul-Lockfile bleibt maßgeblich.
Der Cache liegt auf `emptyDir`: Container-Neustarts behalten ihn, ein neuer Pod
beginnt leer. Der Dienst braucht ausgehenden HTTPS-Zugriff auf Registry und Downloads;
Runner brauchen ihn weiterhin für den Direktbetrieb.

Zur Diagnose `kubectl -n infra-runners get pods -l app=provider-mirror` und
`kubectl -n infra-runners rollout status deploy/provider-mirror` verwenden.
Vor Ablauf des Serverzertifikats in einem Wartungsfenster das TLS-Secret löschen,
`make deploy` ausführen und die CA-Verteilung samt Rollout prüfen. Dies erzeugt
auch eine neue CA; bereits laufende Jobs vorher abschließen lassen.

## Ausführungsvertrag

Der Controller bildet weiterhin einen Hash aus Spec und Generation. Die
Job-Identität enthält zusätzlich die Ressourcen-UID, Aktion und bei Destroy die
Versuchsnummer samt Retry-Token. Der lesbare Namenspräfix ist nur eine Hilfe für
die Diagnose. Existierende Jobs behalten beim Reconcile ihren Namen und ihre
unveränderliche Pod-Vorlage, auch wenn sie vor dem Upgrade erstellt wurden.

Jeder neue Job erhält `PANKO_RESOURCE`: einen unveränderlichen JSON-Snapshot aus
Spec und Metadaten `name`, `namespace`, `uid`, `generation`. `PANKO_HASH` erlaubt dem
Runner, Spec und Generation vor dem Start zu prüfen. Der Runner reicht den Snapshot
ohne fachliches Mapping als `resource.auto.tfvars.json` an HCL weiter. Das Modul
prüft zusätzlich, dass die Identität mit `namespace` und `name` übereinstimmt.
Zugangsdaten stammen ausschließlich aus der technischen Provider-Konfiguration.

Eine Spec-Änderung verändert keinen laufenden Job. Nach dessen Ende verarbeitet
ein weiterer Job die neue Generation. Auch ein Delete wartet auf laufende Applies.
Nur ein erfolgreicher Job für den aktuellen Hash setzt `lastAppliedHash` und Ready.
Sehr alte, vor Einführung des Snapshot-Vertrags gestartete Jobs besitzen noch keinen
Snapshot. Solche Jobs vor einer Umstellung abschließen lassen. Jobs der gesicherten
Python-Portierungsbaseline verwenden denselben Snapshot-Vertrag wie Go.

## State- und Gateway-Isolation

| Namespace | Inhalt | Runner-Zugriff |
| --- | --- | --- |
| `team-a`, `team-b` | CRs, Jobs, Connection-Secrets | Jeweils eigenes Team |
| `panko-state-team-a` | State-Secrets und Lock-Leases von team-a | Nur Runner von team-a |
| `panko-state-team-b` | State-Secrets und Lock-Leases von team-b | Nur Runner von team-b |
| `infra-gateway` | Gateway und `gateway-auth` | Kein Zugriff |
| `infra-runners` | Controller-Hook, Provider-Mirror und TLS-Secret | Kein Zugriff auf Secrets oder Leases |
| `infra-backends` | PostgreSQL und Admin-Secret | Nur `get` auf `postgres-admin` |

States und Leases behalten ihre bisherigen Namen; nur ihr Namespace wechselt.
Kubernetes-RBAC schützt jetzt die Teamgrenze beim State-Zugriff. Die Runner sind
weiterhin privilegierte Infrastrukturkomponenten: Beide lesen dasselbe
PostgreSQL-Adminpasswort und verwalten sämtliche Secrets ihres eigenen
Team-Namespaces. Die Datenbank bietet daher noch keine Isolation gegenüber einem
kompromittierten Runner. Der gemeinsame Gateway-ServiceAccount darf Ressourcen
beider Teams verwalten; der HTTP-Code erzwingt die Token-Team-Zuordnung.

### Vorhandene Installation migrieren

Bei einer frischen Installation ist keine Migration erforderlich. `make deploy`
verweigert den Backend-Wechsel, solange in `infra-runners` alte State-Secrets liegen.
Ein vorhandenes `gateway-auth` wird bei einem Upgrade unter Beibehaltung der Tokens
in `infra-gateway` kopiert. Nach erfolgreichem Gateway-Rollout entfernt Deploy die
alte Gateway-Installation, das alte Auth-Secret und die alte State-Rolle samt Binding.

Bei vorhandenen States:

1. Ein Wartungsfenster herstellen: API-Zugriffe und externe CR-Änderungen aussetzen.
   Laufende Jobs regulär abschließen lassen. Kein State darf parallel beschrieben werden.
2. `make migrate-state` ausführen. Das Werkzeug stoppt den alten Hook und das alte
   Gateway, verweigert die Migration bei aktiven Jobs oder belegten Locks und legt
   eine private Sicherung unter `.local/state-backup-*.json` mit Dateimodus 0600 an.
3. Das Werkzeug kopiert die State-Secrets in die Team-State-Namespaces und die
   Gateway-Tokens nach `infra-gateway`. Bereits vorhandene abweichende Ziel-Secrets
   führen zum Abbruch. Alle Zielinhalte werden vor dem Entfernen der alten States
   verglichen. Freie alte Leases werden nicht kopiert; das Backend erzeugt neue Locks.
4. `make build deploy` ausführen. Erst die neuen Runner dürfen wieder Jobs starten.
   Anschließend `make test` ausführen und das Wartungsfenster beenden.

Die Go-Verwaltungsbefehle übernehmen auch die zufällige Initialisierung von
PostgreSQL-Adminpasswort und Team-Tokens; Bootstrap und Deployment benötigen dafür
keinen Python-Interpreter. Vor dem Löschen kopierter Quell-Secrets prüft Kubernetes
zusätzlich deren UID und ResourceVersion, damit zwischenzeitliche Änderungen zum
Abbruch führen.

Das Migrationswerkzeug bleibt bei einem Abbruch mit gestopptem altem Controller
stehen. Ursache beheben und denselben Befehl erneut ausführen. Bei einer Rückkehr
zum alten Backend zuerst **alle** neuen Ausführungen stoppen und prüfen, ob bereits
neuere States entstanden sind. Die Sicherung nicht ungeprüft über neuere States
kopieren. Sie enthält Zugangsdaten und gehört nicht ins Repository oder in Tickets.

## Fehlgeschlagene Applies und Deletes

Bei `Failed` den Status und die Jobs der Ressourcen-UID prüfen:

```sh
kubectl --kubeconfig .local/kubeconfig -n team-a get db orders -o yaml
kubectl --kubeconfig .local/kubeconfig -n team-a get jobs -l platform.example.com/uid=RESSOURCEN_UID
kubectl --kubeconfig .local/kubeconfig -n team-a logs job/JOB_NAME
```

Ein fehlgeschlagener Apply derselben Generation wird nicht automatisch wiederholt.
Die Ursache beheben und mit `.local/bin/panko --team team-a retry orders` einen
neuen Apply anfordern; alternativ löst eine Spec-Änderung einen neuen Apply aus.

Bei blockiertem Delete beschreibt `status.destroy` den letzten Versuch mit
`attempt`, `jobName`, `retryToken` und `result`. Ein fehlgeschlagener Versuch bleibt
auch nach Job-TTL und Hook-Neustart gesperrt. Nach Behebung der Ursache fordert
`.local/bin/panko --team team-a retry orders` einen neuen Destroy-Versuch an.
Administratoren können alternativ explizit einen neuen Token setzen, zum Beispiel:

```sh
kubectl --kubeconfig .local/kubeconfig -n team-a annotate db orders \
  platform.example.com/retry-destroy=incident-123-attempt-2 --overwrite
```

Jeder weitere Versuch benötigt einen anderen Annotation-Wert. Das Löschen eines
fehlgeschlagenen Jobs allein startet keinen Retry. Annotationen ändern Administratoren
über Kubernetes; die Team-REST-API erlaubt Spec-Patches und die gezielte Retry-Aktion,
aber keine beliebigen Annotationen.
Den Finalizer nicht umgehen: Er gibt die CR erst nach erfolgreichem Destroy und
State-Cleanup frei. Jobs werden nach einer Stunde entfernt; Logs vorher sichern.
Wird ein terminaler Job während einer längeren Controller-Ausfallzeit entfernt,
bevor sein Ergebnis beobachtet wurde, kann die Ausführung erneut geplant werden.

## Cache, Logs und Metriken

`DISCOVERY_TTL_SECONDS` am Gateway bestimmt die Cache-Laufzeit, standardmäßig fünf
Sekunden. `0` deaktiviert den Cache. Parallele Requests teilen sich einen Refresh.
Nach Ablauf führt ein fehlgeschlagener Refresh zu einem Fehler; veraltete Schemas
werden dann nicht weiter ausgeliefert. Neue oder entfernte CRDs werden spätestens
beim ersten erfolgreichen Refresh nach Ablauf sichtbar. Die explizite
RBAC-Synchronisierung mit `make gateway-rbac` bleibt erforderlich.

Gateway-Schreibzugriffe, Controller-Übergänge und Runner-Ergebnisse erzeugen
JSON-Logzeilen mit Ressourcenidentität, Generation und Aktion. Controller und
Runner ergänzen die Job-Identität, der Runner die Laufzeit. Spec, Token und
Zugangsdaten werden nicht in diese strukturierten Felder aufgenommen.

Gateway und Hook stellen im Cluster unter `/metrics` Prometheus-Text bereit:

- `panko_discovery_cache_requests_total{result="hit|miss|error"}` am Gateway.
- `panko_execution_total{action="apply|destroy",outcome="success|failure"}` am Hook.
- `panko_execution_duration_seconds` als Histogramm am Hook, wenn Start- und
  Endzeit des beobachteten Jobs vorhanden sind.
- `panko_blocked_deletions` für länger als `DELETE_ALERT_SECONDS` (Standard 300)
  laufende Löschungen. Seit 120 Sekunden nicht mehr beobachtete Ressourcen fallen
  aus diesem Gauge heraus.

Die Metriken sind pro Prozess und beginnen nach Neustarts neu. Die Job-Deduplizierung
und der Löschungsindex halten jeweils höchstens 8192 Einträge. Bei Überschreitung
können ältere Jobs erneut gezählt werden bzw. ältere Löschungen aus dem Index fallen.
Die Metriken dienen der Betriebsdiagnose, nicht einer revisionssicheren Abrechnung.
Ressourcennamen, UIDs und Teams sind keine Metrik-Labels. Ein externer Prometheus
oder Alertmanager wird durch dieses Projekt nicht installiert.

Technische Grundlagen: [Kubernetes Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/),
[OpenTofu Kubernetes-Backend](https://opentofu.org/docs/language/settings/backends/kubernetes/)
und [mise GitHub Action](https://github.com/jdx/mise-action/tree/v2).


## CLI für den Alltag

`make build-go` baut auch `.local/bin/panko`. Alle Befehle aus dem Repository-Root
aufrufen; in einem zweiten Terminal `make gateway-forward` starten. Die CLI spricht
für Datenbankbefehle mit dem Gateway unter `http://127.0.0.1:8081`.
`--url` oder `PANKO_URL` überschreibt die Adresse. Globale Flags dürfen vor oder
nach dem Befehl stehen.

```sh
.local/bin/panko --team team-a create orders --extensions pgcrypto,citext --connection-limit 20
.local/bin/panko --team team-a wait orders --timeout 5m
.local/bin/panko --team team-a list
.local/bin/panko --team team-a get orders
.local/bin/panko --team team-a get orders --json
.local/bin/panko --team team-a connect orders
.local/bin/panko --team team-a delete orders --wait --timeout 5m
```

`--team` hat Vorrang vor `PANKO_TEAM`. Ohne beides wird genau ein Team aus
`panko.json` übernommen; bei mehreren Teams ist die Auswahl erforderlich.
`--config` kann eine andere Konfigurationsdatei wählen. Der Token kommt aus
`PANKO_TOKEN`, sonst aus `.local/team-tokens.json` (`--tokens-file`). Tokens werden
nicht ausgegeben und nicht an Unterprozesse übergeben.

`create` setzt `owner` standardmäßig auf das Team. Optional sind `--owner TEAM`,
`--extensions a,b`, `--connection-limit N` und `--state-backend REF`. Alternativ
liest `create orders -f spec.json` ein reines Spec-Objekt ohne `name`/`spec`-Hülle;
fehlender `owner` wird auch dort ergänzt. Datei und Spec-Flags sind nicht kombinierbar.
Die Schema-Prüfung und die übrigen Defaults bleiben beim Gateway/Kubernetes.
Es gibt kein implizites Patchen bestehender Ressourcen.

`get` und `list` zeigen Phase, Meldung, letzte Ausführung mit Code, Versuch und
Endzeit, Änderungszahlen und den gebundenen State-Backend-Typ. `--json` liefert die
API-Sicht. Fehlerantworten zeigen auch `message` und die einzelnen `errors`.
`wait` endet bei Ready mit 0, bei Failed mit Code/Meldung und Exitcode 1, bei
Zeitüberschreitung mit 2. Standard sind fünf Minuten. Auch ungültige Aufrufe enden
mit 2; andere Betriebsfehler mit 1. `delete --wait` wartet auf das Verschwinden der
Ressource und bricht bei einem fehlgeschlagenen Destroy mit 1 ab.

Nach Behebung eines Fehlers fordert `.local/bin/panko --team team-a retry orders`
über die gezielte Gateway-Aktion einen neuen Apply- oder Destroy-Versuch an.
HTTP 409 wird als „nothing to retry“ gemeldet. Ein Retry wartet nicht selbst auf
Abschluss: danach `wait` beziehungsweise bei einem Destroy `delete --wait` verwenden.

`connect` liest ausschließlich das in `status.connectionSecretRef` benannte Secret
im Team-Namespace über den lokalen Kubernetes-Zugang. `--kubeconfig` hat Vorrang
vor `KUBECONFIG`, Standard ist `.local/kubeconfig`; es gibt keinen impliziten
In-Cluster-Zugang. Der Gateway bekommt dafür keine zusätzlichen Secret-Rechte.
Standardausgabe ist nur die PostgreSQL-URI. `--format env` liefert shell-quotierte
`export PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSWORD`-Zeilen, `--format json` die
dekodierten Secret-Felder. Alle Formate enthalten bewusst Zugangsdaten; nicht in
Logs oder Tickets übernehmen.

`.local/bin/panko doctor` zeigt pro Prüfung `ok` oder `fail` mit Hinweis und endet
bei Fehlern mit 1. Geprüft werden Konfiguration, Kubernetes-Erreichbarkeit,
Hook-/Gateway-/Metacontroller-Rollouts, Modul-Secrets in allen Team-Namespaces,
Gateway-Tokens für alle Teams, eine gültige lokale Token-Datei mit Modus 0600 und
bei aktiviertem S3 dessen StatefulSet. Dies prüft vorhandene Modul-Secrets, keinen
Git-Zugriff aus einem Runner. Ein nicht erreichbares `/healthz` am lokalen Gateway
ist nur `warn`, damit ein fehlendes Port-Forwarding nicht die Clusterdiagnose scheitern lässt.
`doctor` benötigt einen Owner-Kubernetes-Zugang mit Leserechten für diese Objekte.

Einen einzelnen Team-Token rotieren:

```sh
.local/bin/admin --kubeconfig .local/kubeconfig rotate-token --team team-a
```

Der Befehl ersetzt genau diesen Token in `infra-gateway/gateway-auth` und in der
lokalen Datei (Modus 0600); andere Team-Tokens bleiben jeweils unverändert.
`--tokens-file` und `--config` werden auch hier unterstützt. Vorhandenes Secret und
lokale Datei sind erforderlich. Der Gateway liest die Datei je Request; die
Secret-Volume-Aktualisierung durch Kubernetes kann kurz dauern. Ein gesetztes
`PANKO_TOKEN` muss anschließend lokal aktualisiert oder entfernt werden. Falls das
Cluster-Update erfolgreich war, die lokale Ersetzung aber scheitert, stellt
`admin ... ensure-gateway-auth` die lokale Datei aus dem Secret wieder her.
