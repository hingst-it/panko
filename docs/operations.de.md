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

## Engine und Modul

Runner-Jobs setzen sich beim Start aus drei Teilen zusammen. Der Init-Container
`engine` kopiert das OpenTofu-Binary aus `ENGINE_IMAGE` in ein gemeinsames Volume.
Der Init-Container `module` holt das Modul per git-sync aus `MODULE_REPO` zum Commit
`MODULE_REVISION`. Danach führt der Runner das kopierte Binary gegen dieses Modul aus.
Beide Images sind per Digest festgelegt, das Modul per vollständigem Commit-SHA; der
Hook startet nicht, wenn die Revision ein Branch oder Tag ist.

Provider lädt OpenTofu beim Start aus der Registry. `-lockfile=readonly` prüft sie
gegen das Lockfile des Moduls, das deshalb Prüfsummen aller Runner-Plattformen
enthalten muss. Jobs benötigen damit Netzwerkzugriff auf GitHub und die
Provider-Registry.

Ein Modul-Update bedeutet: neuen Commit-SHA als `MODULE_REVISION` in
`controller/deployment.yaml` eintragen und `make deploy` ausführen. Jede Database
wird danach einmal mit der neuen Revision angewendet. Der Status hält mit
`lastAppliedRevision` fest, welche Revision zuletzt erfolgreich angewendet wurde;
Destroy verwendet genau diese Revision. Ressourcen ohne erfasste Revision werden
beim ersten Deployment dieses Aufbaus einmal angewendet.

Der Deploy-Key ist nur lesend für das Modul-Repository freigegeben. `make deploy`
verteilt ihn mit `admin module-secret` als Secret `panko-module-git` in jeden
Team-Namespace, zusammen mit den Host-Schlüsseln aus `cluster/module-known-hosts`.
Diese stammen aus GitHubs API (`gh api meta`) und müssen bei einem Schlüsselwechsel
von GitHub aktualisiert werden.

Ein separates Lab lässt sich mit `make CLUSTER_NAME=infra-lab-go KUBECONFIG="$PWD/.local/go-fresh-kubeconfig" cluster-up bootstrap build deploy` erstellen. Für alle weiteren
Make-Aufrufe dieselben Werte verwenden. `make cluster-down` entfernt ausschließlich
den ausgewählten Cluster.

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
| `infra-runners` | Controller-Hook | Kein Zugriff auf Secrets oder Leases |
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
Die Ursache beheben und die Spec ändern, um einen neuen Apply anzufordern.

Bei blockiertem Delete beschreibt `status.destroy` den letzten Versuch mit
`attempt`, `jobName`, `retryToken` und `result`. Ein fehlgeschlagener Versuch bleibt
auch nach Job-TTL und Hook-Neustart gesperrt. Nach Behebung der Ursache explizit
einen neuen Token setzen, zum Beispiel:

```sh
kubectl --kubeconfig .local/kubeconfig -n team-a annotate db orders \
  platform.example.com/retry-destroy=incident-123-attempt-2 --overwrite
```

Jeder weitere Versuch benötigt einen anderen Annotation-Wert. Das Löschen eines
fehlgeschlagenen Jobs allein startet keinen Retry. Annotationen ändern Administratoren
über Kubernetes; die Team-REST-API erlaubt weiterhin ausschließlich Spec-Patches.
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
