# Auswertung: PoC vereinheitlichter Panko-Server und vereinfachte Installation

Branch `poc/unified-server-dev-install`, Auftrag: `POC.md` (lokale Aufgabenbeschreibung, nicht im Repository).
Keine Veröffentlichung, kein Merge nach `main`. Stand: 2026-10-28.

## Ergebnis

Der Ansatz ist technisch tragfähig. Eine gemeinsame Binary `panko-server` startet alle
Laufzeitrollen; Metacontroller v4.17.6 läuft als festgelegte Go-Abhängigkeit in der
Controller-Rolle und ruft den bestehenden Hook über Loopback-HTTP auf. Ein versioniertes
Helm-Chart ist der einzige Installationsweg für `panko install`, `panko dev up`, `make deploy`
und GitOps. `panko dev up` erzeugt aus einem beliebigen Verzeichnis ohne Checkout, Make oder
Image-Build ein dediziertes k3d-Lab, ohne Hintergrundprozess und ohne die globale Kubeconfig
zu verändern. Die bestehenden E2E-Suiten bestehen unverändert in ihrem fachlichen Umfang
(nur Image- und Adressanpassungen der Testfixtures).

## Akzeptanzkriterien des Auftrags

Die Kriterien stammen wörtlich aus der lokalen Aufgabenbeschreibung `POC.md`.

| Kriterium | Stand | Beleg |
| --- | --- | --- |
| Umsetzung vollständig auf dem separaten PoC-Branch | erfüllt | Branch `poc/unified-server-dev-install`, kein Merge, kein Push |
| Gemeinsame Server-Binary startet alle vorgesehenen Rollen | erfüllt | `panko-server gateway/controller/endpoint/mirror/runner/agent/setup`; alle laufen in Lab, E2E und GitOps-Läufen |
| Metacontroller läuft integriert in der Controller-Rolle | erfüllt | kein Namespace `metacontroller`; CompositeController über Loopback, Metriken auf `:9999` |
| Rollen behalten getrennte ServiceAccounts und Berechtigungen | erfüllt | Chart-Tests, `kubectl auth can-i`, `rbac_e2e` |
| `panko install` funktioniert in einem dedizierten vorhandenen Testcluster | erfüllt | CI-Weg `make cluster-up build deploy`, Migration, Wegwerf-Cluster |
| `panko dev up/status/down` funktioniert ohne Checkout beim Anwender | erfüllt | Läufe aus einem leeren Verzeichnis ohne `make`, Go oder k3d auf `PATH` |
| Wiederholter Start und Neustarts erhalten Ressourcen und State | erfüllt | Fingerprints vor/nach Cluster-Neustart, wiederholtem `dev up`, Migration |
| Ein Ressourcenlebenszyklus einschließlich Fehler, Retry und Destroy ist geprüft | erfüllt | Dev-Lab (Provider-Fehler, `retry`, `delete`) und `failures_e2e` |
| Helm-Lint und Rendering-Prüfungen bestehen | erfüllt | `make lint-chart`, `internal/chart`-Tests |
| GitOps-Rendering ist reproduzierbar und benötigt keine verborgenen CLI-Schritte | erfüllt | byteidentisches Rendering; vorgerenderte Manifeste, Argo CD und Flux ohne `panko install` |
| Argo-CD- und Flux-Beispiele sind enthalten; der Validierungsumfang ist dokumentiert | erfüllt | `examples/gitops`, [gitops.md](gitops.md#validation-scope) |
| Fremde Cluster, Kontexte und Daten bleiben unverändert | erfüllt, mit Vorbehalt | globale Kubeconfig byteidentisch; fremde k3d-Cluster und Metacontroller werden abgelehnt. Vorbehalt: die nicht versionierte Datei `tsystems.apikey` im Repository-Wurzelverzeichnis fehlte nach der Sitzung; kein Befehl dieser Arbeit hat sie angesprochen, die Ursache ist ungeklärt |
| Relevante Unit-, RBAC- und E2E-Prüfungen bestehen | erfüllt | `make test-unit lint lint-go`, E2E-Shards a/b/c, Observability-E2E mit Rollenmetriken |

## Was funktioniert und wie es reproduziert wird

| Bereich | Umsetzung | Nachweis |
| --- | --- | --- |
| Gemeinsame Binary | `cmd/panko-server`, Rollen in `internal/roles`: `gateway`, `controller`, `endpoint`, `mirror`, `runner`, `agent`, `setup` | Unit-Tests `internal/roles`; alle Rollen laufen im Lab, `agent` im Remote-Runner-E2E |
| Eingebetteter Metacontroller | `internal/roles/controller.go`, `server.New` aus Metacontroller v4.17.6, Hook auf `127.0.0.1:8090` | Lab: `database-controller` wird synchronisiert, Lebenszyklus vollständig |
| Getrennte ServiceAccounts | Controller `panko-controller`, Gateway `platform-gateway`, Endpoint `runner-endpoint`, Setup `panko-setup`, Runner `tofu-runner` je Team; Mirrors ohne Token | `internal/chart`-Tests, `kubectl auth can-i` im Lab, `test/rbac_e2e.py` |
| Helm-Chart | `charts/panko` (Version `0.1.0-poc.1`): CRDs, RBAC, Rollen, Setup-Job, Lab-PostgreSQL/S3 | `make lint-chart`, `internal/chart`-Tests |
| `panko install` | `internal/install`: Ziel ausweisen, Rechte und Konflikte prüfen, CRDs anwenden, Helm-SDK-Install/Upgrade, Readiness | Lab über `make deploy` (CI-Weg) und `panko dev up` |
| `panko dev up/status/down` | `internal/devlab`: Docker, k3d (privat mit Prüfsumme), eigener Cluster und Kubeconfig, Port-Mapping statt Port-Forward | manueller Lauf ohne Checkout, Unit-Tests mit gefälschtem `docker`/`k3d` |
| GitOps | `examples/gitops`: Werte, Argo CD, Flux, vorgerenderte Manifeste; [gitops.md](gitops.md) | Rendering reproduzierbar, keine Secrets; Argo CD/Flux nicht angewendet |
| `panko admin` | bisherige `panko-admin`-Befehle, zusätzlich `--config cluster` | `test/admin_e2e.py` u. a. über den Kompatibilitäts-Wrapper `admin` |

Reproduktion:

```sh
# Unit-, Chart- und Lint-Prüfungen
make test-unit lint lint-go
# Lab ohne Checkout (Images einmalig bauen, z. B. auf einem anderen Rechner)
docker build -t panko-server:dev -f Dockerfile.server .
docker build -t panko-module-mirror:dev -f cluster/Dockerfile.module-mirror .
go build -o /tmp/panko ./cmd/panko
cd "$(mktemp -d)" && /tmp/panko dev up --server-image panko-server:dev --module-mirror-image panko-module-mirror:dev
# CI-Weg mit E2E-Suiten
make cluster-up build deploy CLUSTER_NAME=panko-poc-e2e
make test-shard SHARD=a   # ebenso b und c
```

### Geprüfte Abläufe

Alle Läufe am 2026-10-28 auf macOS ARM64 mit Docker 29.2, k3d 5.9.0, k3s v1.35.5, jeweils in
eigenen Wegwerf-Clustern; vorhandene Cluster und Kubeconfigs blieben unverändert.

| Prüfung | Ergebnis |
| --- | --- |
| `make test-unit lint lint-go` (Go-Tests mit Race Detector, `helm lint --strict`, reproduzierbares Rendering ohne Secrets, gofmt, vet, ruff) | bestanden |
| `node --test .github/scripts/release-gate.test.cjs` | 20/20 bestanden |
| `panko dev up` aus einem Verzeichnis ohne Checkout, `PATH` ohne k3d: Zustimmung abgelehnt → Abbruch ohne Änderung; mit `--install-k3d` private Installation, SHA-256 geprüft | bestanden |
| Lebenszyklus im Dev-Lab: `doctor` ohne `panko.json`, Create mit absichtlich falschem PostgreSQL-Passwort → `Failed provider`, Korrektur, `retry --wait` → Ready (Versuch 2), `connect`, `delete --wait` → Ressource, State und Connection-Secret entfernt | bestanden |
| Neustart: `k3d cluster stop`, erneutes `dev up` startet und aktualisiert das Lab (Revision 3); State-Fingerprint, Token-Datei und Ressource unverändert | bestanden |
| `dev down` mit falscher Bestätigung → keine Änderung; mit Lab-Namen → nur dieses Lab gelöscht | bestanden |
| Globale `~/.kube/config` | Inhalt und Kontexte unverändert; im ersten Lauf schrieb `k3d cluster delete` die Datei jedoch neu (gleicher Inhalt, neue Änderungszeit). Behoben: k3d läuft mit einer lab-eigenen `KUBECONFIG`; spätere Läufe ließen die Datei unberührt |
| CI-Weg `make cluster-up build deploy` (`panko install` in einen vorhandenen k3d-Cluster), danach E2E-Shards a, b, c (RBAC, Lebenszyklus, Gateway, CLI, UI, Egress, State-Backends, Fehler/Retry, Drift, Admin-Migration, Remote-Runner, Runner-Smoke, Provider-Mirror, Observability) | alle bestanden (~16 min) |
| Erneutes `make deploy` auf dem getesteten Lab: CRDs angewendet, Setup-Job mit gleichem Namen neu erzeugt und abgeschlossen, Revision 2 | bestanden |
| `kubectl auth can-i` je Rolle: Controller/Gateway/Setup/Endpoint ohne fremde Secrets, Runner nur im eigenen State-Namespace, Setup ohne Workloads, Endpoint nur `runs/status` | wie vorgesehen |
| Migration einer v0.0.1-Installation (`make bootstrap build deploy` von `main`) mit Ressource, siehe [Migration](#migration) | bestanden nach drei Korrekturen |
| Abgebrochenes `panko install` (Prozess während des Helm-Wartens beendet): nächster Lauf markiert die hängende Revision als fehlgeschlagen und installiert erneut | bestanden |
| Eigener Wegwerf-Cluster: nur lesende Identität → Ablehnung mit allen fehlenden Rechten; vorgerenderte Manifeste mit `kubectl apply --server-side`; Ablehnung von Legacy-States und entfernten Pools; Umstellung auf Helm-Release mit `--take-ownership`; `helm uninstall` behält Daten | bestanden |
| Abgebrochenes `panko dev up` (SIGTERM während des Helm-Wartens), erneutes `dev up` ohne Port-Angabe: Lab und Port wiederverwendet, Revision `pending-install` fortgesetzt, danach `dev status`, Lebenszyklus in `team-b`, `dev down --yes`; globale Kubeconfig vor und nach dem Lauf byte-identisch (Änderungszeit, Größe, SHA-256) | bestanden |

### Start, Shutdown, Readiness, Logs und Metriken je Rolle

Shutdown geprüft durch Löschen des Pods und Auswertung des letzten Containerzustands aus einem
Watch: alle Rollen-Deployments mit Exit-Code 0 und Grund `Completed`, also ohne SIGKILL.

| Rolle | Start | Shutdown | Readiness | Logs | Metriken |
| --- | --- | --- | --- | --- | --- |
| `controller` | Deployment `database-hook`, SA `panko-controller`; wartet auf `panko-installation` | SIGTERM: Manager stoppt, danach leeren die Hook-Listener laufende Anfragen; Grace 40 s; geprüft: Exit 0 nach ~1 s | `/readyz`: Hook-Listener aktiv und Cache-Sync (geprüft: 200); `/healthz` Liveness | Hook-Ereignisse JSON auf stdout (`event`), Metacontroller zap-JSON auf stderr | `:8080/metrics` Hook (E2E Observability), `:9999/metrics` controller-runtime (geprüft) |
| `gateway` | Deployment, SA `platform-gateway`; wartet auf `gateway-auth` | SIGTERM: geordnetes Herunterfahren (`internal/serve`, Unit-Test); geprüft: Exit 0 nach ~1 s | `/healthz` Readiness und Liveness | JSON-Schreibprotokoll | `/metrics` Discovery-Cache (geprüft) |
| `endpoint` | Deployment, SA `runner-endpoint`; wartet auf TLS-Secret | SIGTERM über `internal/serve`; geprüft: Exit 0 nach ~1 s | `/healthz` über HTTPS | JSON | `/metrics`: `panko_endpoint_requests_total` und Dauer je Protokolloperation (neu, E2E Observability) |
| `mirror` | Deployment ohne Token, PVC-Cache | SIGTERM über `internal/serve`; geprüft: Exit 0 nach ~1 s | `/healthz` über HTTPS | Warnungen bei Ausfällen | `/metrics`: `panko_mirror_requests_total` je Index/Version/Archiv (neu, E2E Observability) |
| `runner` | Job je Ausführung, SA `tofu-runner` des Teams | SIGTERM wird an OpenTofu weitergereicht (Lock-Freigabe; Unit-Tests `internal/runner`, E2E-Abbruch `interrupted`) | Job-Status und Termination-Message | JSON auf stdout | über die Ausführungsmetriken des Hooks |
| `agent` | außerhalb des Clusters (Container, systemd, anderer Cluster) | SIGTERM, Grace 90 s in den Beispielen (Remote-E2E beendet einen Agenten während einer Ausführung) | `agent check` | stdout | keine |
| `setup` | Job mit Digest-Namen, SA `panko-setup` | SIGTERM bzw. Zeitlimit 5 min | Job `Complete` | `ok <Schritt>`, nie Werte | keine (Job-Status) |

## Metacontroller-Einbettung und Integrationsgrenzen

Der Prototyp (separates Go-Modul, nur `server.New` aufrufen) war der erste Schritt; er hat
die folgenden Grenzen gezeigt, die in die Umsetzung eingeflossen sind:

- **Modulpfad.** Metacontrollers `go.mod` heißt `module metacontroller`, die Tags `v4.x` haben
  kein `/v4`-Suffix. `go get` ist deshalb nicht möglich. Eingebunden wird es mit
  `require metacontroller v0.0.0` und `replace metacontroller =>
  github.com/metacontroller/metacontroller v1.5.21-0.20261015191212-be3f3f973ddc`, der
  Pseudo-Version für den Commit `be3f3f97` des Tags `v4.17.6`; `go.sum` legt den Inhalt fest.
  Renovate erkennt diese Bindung nicht; Updates sind manuell (Commit des neuen Tags auflösen).
- **Abhängigkeiten.** Metacontroller v4.17.6 und Helm v4.3.1 verwenden dieselben
  `k8s.io/*` v0.37.1 und controller-runtime v0.25.2 wie Panko. Künftig müssen alle drei im
  Gleichschritt aktualisiert werden; ein Versatz der Kubernetes-Bibliotheken blockiert Updates.
- **API-Fläche.** Eingebettet wird `pkg/server.New(ctx, options.Configuration)`, das einen
  controller-runtime-Manager liefert. Nicht über die Konfiguration erreichbar sind u. a.
  `LeaderElectionReleaseOnCancel` und eigene Health-Checks; Panko setzt
  `HealthProbeBindAddress: "0"` und bedient `/healthz`/`/readyz` selbst (Readiness = Hook-Listener
  aktiv und Cache-Sync des Managers). Metacontroller nutzt globale Logger (`logging.Logger`,
  klog, controller-runtime); es kann nur eine Instanz je Prozess geben, Logs sind
  zap-JSON auf stderr neben Pankos JSON-Logs auf stdout.
- **Hook-Anbindung.** Der Hook bleibt unverändert und wird über `http://127.0.0.1:8090` im selben
  Pod aufgerufen. `/sync`, `/customize` und `/finalize` sind damit nicht mehr aus dem
  Pod-Netz erreichbar; der Port 8080 bedient nur `/healthz`, `/readyz` und `/metrics`. Eine
  direkte In-Process-Anbindung ohne HTTP wäre möglich, wurde aber bewusst nicht umgesetzt.
- **Fremde Controller.** Die eingebettete Instanz verwaltet nur CompositeController mit dem
  Label `platform.example.com/controller=panko` (`--target-label-selector`). Ein separat
  laufender Metacontroller ohne Selektor würde Pankos Controller trotzdem ebenfalls
  bedienen; `panko install` verweigert deshalb einen Metacontroller im Namespace
  `metacontroller`.
- **RBAC.** Statt der Wildcard-Rechte des Metacontroller-Charts hat die Controller-Rolle eine
  explizite ClusterRole. Im Lab fehlte zunächst `list/watch` auf `namespaces`
  (Metacontroller beobachtet Namespaces für Selektoren verwandter Ressourcen); ergänzt.
- **CRDs.** Die Metacontroller-CRDs stammen unverändert aus v4.17.6 und liegen in
  `charts/panko/crds/metacontroller.yaml`. Gehören sie einer anderen Installation, lässt
  `panko install` sie unverändert.
- **Lizenz.** Metacontroller steht unter Apache-2.0 wie Panko.

## Prüfung der Dokumentation

Geändert bzw. neu: README, `docs/quickstart.md`, `docs/agent-install.md`, `docs/installation.md`,
`docs/gitops.md`, `docs/local-lab.de.md`, `docs/operations.de.md`, `docs/remote-runners.de.md`,
`docs/state-backends.de.md`, `docs/go-porting.de.md`, `docs/releases.md`, `AGENTS.md`,
`CHANGELOG.md`, `charts/panko/README.md`, `examples/gitops/*`. Alle relativen Links und Anker
lösen auf; keine Verweise auf entfernte Dateien, Make-Ziele oder Binaries (Ausnahme: der als
historisch markierte Python-Rückwechsel in `go-porting.de.md`).

| Aussage | Beleg |
| --- | --- |
| `dev up` ohne Checkout, private k3d-Installation mit Prüfsumme, Ablehnung ohne Zustimmung | Lauf ohne Checkout, `PATH` ohne k3d |
| `dev up` wiederholt, Neustart, Fortsetzen nach Abbruch, Port wiederverwendet | manuelle Läufe (siehe Tabelle oben) |
| Fremder Cluster gleichen Namens bzw. fremdes Owner-Label wird abgelehnt und nicht verändert | Unit-Tests `internal/devlab` |
| Belegter Port wird vor jeder Änderung abgelehnt | Lauf mit belegtem Port: kein Cluster, kein Zustand |
| `dev status` ohne Secrets; `dev down` nur nach Bestätigung, nur das eigene Lab | Unit-Test und manuelle Läufe |
| Globale Kubeconfig unverändert | Vergleich von Änderungszeit, Größe, SHA-256 |
| `panko install` nennt Ziel und Kontext, prüft Rechte | Lauf mit nur lesender Identität: alle fehlenden Rechte genannt, nichts angelegt |
| Ablehnung: Installation ohne Helm, separater Metacontroller, Legacy-States, entfernte Pools | je ein Lauf gegen einen echten Cluster |
| CRDs vor jedem Upgrade, Metacontroller-CRDs fremder Installationen bleiben | Ausgaben der Läufe; Übernahme bei `--take-ownership` |
| Setup-Job bei jedem `panko install` erneut, Credentials unverändert | erneutes `make deploy`; Token-Hash vor/nach |
| Abbruch während des Helm-Wartens wird fortgesetzt | `panko install` und `panko dev up` abgebrochen und erneut gestartet |
| Migration mit `--take-ownership` | v0.0.1-Lab, siehe [Migration](#migration) |
| GitOps: reproduzierbares Rendering, keine Secrets, vorgerenderte Manifeste, Reihenfolge CRDs → Rest | `make lint-chart`, Chart-Tests, Lauf mit `kubectl apply --server-side` |
| `helm uninstall` behält Namespaces, CRDs, StatefulSets, PVCs und Secrets | Lauf gegen echten Cluster |
| Argo CD: Beispiel-`Application` (nur Repository-URLs ersetzt) mit `ServerSideApply`, `SkipDryRunOnMissingResource`, zwei Quellen, Self-Heal | Argo CD v3.5.6 im Wegwerf-Cluster, Git über SSH: Synced/Healthy, Credentials erzeugt, Egress aktiv, Lebenszyklus, kein Drift nach Hard Refresh, Setup-Job ohne Wiederholung, Self-Heal stellt Replikas wieder her. Gefunden und behoben: dauerhaftes `OutOfSync` des PostgreSQL-StatefulSets |
| Flux: Beispiel-`GitRepository`/`HelmRelease` | Flux v2.10.0 im Wegwerf-Cluster, Git über SSH: `HelmRelease` Ready nach 55 s, CRDs per `crds: CreateReplace` angelegt, Release-Record in `panko-system`, Setup-Job, Lebenszyklus, geänderter Wert über `valuesFrom` ausgerollt (Gateway 2 Replikas), Tokens unverändert |
| Pruning-Schutz beim Löschen der Anwendung | Argo CD (`Application` mit `resources-finalizer`) und Flux (`HelmRelease` gelöscht, Release deinstalliert): Namespaces, CRDs, PostgreSQL-StatefulSet, PVCs, Secrets und eine Ready-Database bleiben, nur die zustandslosen Deployments verschwinden. Gefunden und behoben: Die Deinstallation löschte auch den CompositeController; ohne laufenden Controller blieb er mit dem Metacontroller-Finalizer `metacontroller.io/parent-finalizer-cleanup` in `Terminating` (Flux wartete 318 s und meldete einen Fehler), und eine spätere Neuinstallation hätte die Destroy-Finalizer aller Databases entfernt. Der CompositeController wird jetzt behalten; danach: Deinstallation in 11 s ohne Fehler, Database behält ihren Finalizer, Neuinstallation in 15 s Ready |
| Bilder per `docker save`/`docker load` übertragen | Archiv (23 MB) gespeichert, Images gelöscht, geladen: identische Image-IDs; Argo-CD- und Flux-Cluster liefen mit diesen Images |
| Zertifikatserneuerung (TLS-Secrets löschen, `make deploy`, Mirror und Endpoint neu starten) | neue CAs, Team-ConfigMaps aktualisiert, folgender Apply lud alle Provider über den Mirror ohne Rückfall |
| SSH-Modul über `module.sshSecret` | SSH-Git-Server im Lab-Netz: Mirror klont per SSH (sshd-Log), Ressource Ready, Schlüssel nur in `infra-runners`, kein Job-Pod mountet ihn |

## Review-Befunde (Codex, PR #70)

Drei Review-Runden; jeder Befund wurde am Code bzw. durch Rendering nachvollzogen, alle trafen zu.

| Befund | Umsetzung | Beleg |
| --- | --- | --- |
| P1 Entfernen eines Teams entzieht Runner-Rechte, Ressourcen bleiben | Runner-Identität, Team-/State-RBAC, Endpoint-Rechte und BackendProfile werden behalten (Keep-/Pruning-Annotationen), gemeinsame Bindings je Team; `panko install` verweigert das Entfernen eines Teams mit Ressourcen | Chart-Test `TestTeamRemovalKeepsExecutionAccess`, Unit-Test; live: Verweigerung, nach `helm upgrade` ohne das Team bleiben die Rechte, Destroy gelingt |
| P1 GitOps umgeht die Pool-Prüfung | Controller hält Pool-Definitionen in `panko-runner-pools-retained`; gebundene, nicht mehr konfigurierte Pools bleiben ausführbar. Folgerunden: Aufzeichnung nur wachsend (kein Löschen nach Momentaufnahme), Schreiber geordnet nach Deployment (UID, Erstellzeit) und Rollout-Revision, Owner per UID geprüft | Unit-Tests für Entfernen, ältere Replika, neu erstelltes Deployment, überlappende Neuerstellung; live (siehe unten) |
| P1 Zweiter Installer übernimmt laufende Operation | Lease `panko-system/panko-install`; Wiederaufnahme nur nach nachweislich abgebrochenem `panko install`, sonst `--recover-pending`. Folgerunden: Prüfung vor jeder Änderung, Abbruch aller Änderungen bei Verlust der Lease, Watchdog ab geschriebenem `renewTime` unabhängig von hängenden Anfragen, zu langsame Übernahme abgelehnt | Unit-Tests (Übernahme, API weg, API hängt, langsame Übernahme); live: zweiter Lauf wartet, echte `pending-upgrade`-Revision eines `helm upgrade` verweigert ohne eine Änderung |
| P1 Zertifikatserneuerung ohne Koordination | Stabile CA (Schlüssel nur für Setup), Neuladen in Mirror und Endpoint ohne Neustart, täglicher Setup-CronJob; Migration bestehender Installationen gestaffelt (CA-Bündel bis zur nächsten Erneuerung) | Unit-Tests inkl. Migration vom alten Verfahren; live: neues Zertifikat nach ~20–40 s ausgeliefert, gleiche CA, gleicher Pod |
| P2 S3-Image-Änderung auf unveränderlichem Job | Job-Name aus Teamliste und gerendertem Pod-Template (erste Fassung hashte wegen `.Files` einen leeren String) | Chart-Tests für Image-, Team- und Template-Änderung; live: neuer Job-Name beim Image-Wechsel über `helm upgrade` |
| P2 Pull-Secrets erreichen Runner-Jobs nicht | `imagePullSecrets` an den `tofu-runner`-ServiceAccounts | Chart-Test; live: privates Registry mit Passwort, Runner-Job zieht `panko-server` nur über das Pull-Secret |
| Zusatz: CRDs ohne Pruning-Schutz | Argo-CD- und Flux-Annotationen an allen CRDs | Chart-Test `TestCRDsAreProtectedFromPruning` |
| Folgebefund aus dem Live-Test | Helm-SDK verwendete den Feldmanager `panko`; ein späteres `helm upgrade` scheiterte an Konflikten. Jetzt Feldmanager `helm` wie das Helm-CLI | live: `helm upgrade` nach `panko install` ohne Konflikte |
| Folgebefund aus dem Live-Test | Chart setzte kein `MODULE_REPO`; Runner-Pools ohne eigenes Modul liefen nie | Chart-Test; live: Run für Pool ohne Modul |
| Folgebefund aus dem Live-Test | Ungültige Installationswerte übersprangen still alle Vorabprüfungen | Unit-Test; Ablehnung vor jedem Clusterzugriff |

Live geprüft in Wegwerf-Clustern (k3d, Build mit allen Korrekturen):

- Privates Registry mit Passwort (TLS, anonymer Abruf 401), Image nur dort: Installation, Setup-Job,
  alle Rollen und der Runner-Job einer Database ziehen `panko-server` über die Pull-Secrets.
- Zwei gleichzeitige `panko install`: der zweite wartet auf den ersten, beide nacheinander erfolgreich.
- Echte `pending-upgrade`-Revision eines hängenden `helm upgrade`: `panko install` endet mit Fehler,
  ohne CRD-, Job- oder Deployment-Änderung; danach regulär fortsetzbar.
- TLS-Secret des Mirrors gelöscht, `panko install`: neue Seriennummer wird nach 20–60 s ausgeliefert,
  gleiche CA, gleicher Pod; der CronJob läuft manuell ausgelöst durch.
- `helm upgrade` nach `panko install` ohne Feldmanager-Konflikte; S3-Image-Wechsel erzeugt einen
  neuen Onboarding-Job, der abschließt.
- Pool `edge` mit gebundener Database (S3-StateBackend): `panko install` verweigert das Entfernen;
  ein `helm upgrade` entfernt ihn trotzdem, der Controller meldet `runner_pool_retained`, und nach
  einer Spec-Änderung entsteht ein neuer Run (Phase `Applying`).
- Team entfernt über `helm upgrade`: Gateway-Rolle weg, Runner-Identität und State-RBAC bleiben,
  Destroy der verbliebenen Database samt State-Bereinigung gelingt; `panko install` hatte das
  Entfernen verweigert.
- Dabei gefunden und behoben: Pools ohne eigenes Modul konnten mit dem Chart nie ausgeführt werden
  (`MODULE_REPO` fehlte); ungültige Installationswerte ließen die Vorabprüfungen still aus und
  werden jetzt vor jedem Clusterzugriff abgelehnt.

## Bekannte Einschränkungen und offene Entscheidungen

- **Images und Chart sind nicht veröffentlicht.** Der Release-Workflow baut `panko-server`
  und `panko-module-mirror`, wurde aber nicht ausgeführt; ein OCI-Chart fehlt. Für GitOps
  müssen Images in eine eigene Registry geschoben werden.
- **Namen.** Die Controller-Rolle läuft weiterhin im Deployment `infra-runners/database-hook`,
  weil Pause/Resume, Sicherung, Migrationen und Tests diesen Namen verwenden. Eine
  Umbenennung (z. B. `panko-controller`) ist offen.
- **Hook im Pod mit API-Token.** Der Hook stellt keine API-Anfragen, läuft aber jetzt im
  selben Prozess wie Metacontroller und damit mit dessen ServiceAccount. Die frühere
  Prozessgrenze (Hook ohne Token) entfällt. Offen: Hook als zweiter Container im Pod
  (getrennter Prozess, weiterhin Loopback) oder bewusste Annahme.
- **Feste Secret-Namen.** Vorhandene Secrets werden über feste Namen referenziert, nicht über
  frei wählbare Werte (`secrets.generate=false` prüft sie). Konfigurierbare Namen würden
  Änderungen in Gateway, Runner, Endpoint und Admin-Befehlen erfordern.
- **Setup-Job statt Helm-Hooks.** Credentials entstehen in einem normalen Job, dessen Name einen
  Digest seiner Eingaben trägt; Pods warten bis dahin in `ContainerCreating`. Helm-Hooks
  hätten Namespaces und RBAC außerhalb des Releases benötigt. Bei GitOps läuft der Job nur
  bei geänderten Eingaben; ein gelöschtes Credential entsteht erst nach Löschen des Jobs
  neu. `panko install` startet ihn bei jedem Lauf neu.
- **CA-Austausch** (nicht die Erneuerung der Serverzertifikate) bleibt ein manueller Schritt, der
  Setup-Job warnt ein Jahr vor Ablauf; Agenten brauchen dann die neue CA.
- **Gateway-RBAC** wird aus `gateway.exposedResources` gerendert. Zur Laufzeit ergänzte CRDs
  brauchen weiterhin `panko admin gateway-rbac`; ein Upgrade setzt diese Ergänzungen zurück.
- **Modul-Rollout.** Controller und Modul-Mirror rollen im selben Upgrade aus; ein weniger als
  eine Minute alter Commit kann für die ersten Jobs fehlen (`module_fetch`, dann `retry`).
  Das alte Skript startete erst den Mirror und dann den Hook.
- **Plattformen.** `panko dev` unterstützt macOS und Linux mit Docker; Podman, Windows und
  VM-Verwaltung sind ausgeschlossen. k3d wird bei Bedarf in v5.9.0 installiert; die
  Prüfsummen sind im CLI festgelegt und müssen bei einem k3d-Update angepasst werden.
- **Argo CD und Flux** wurden je einmal in einem Wegwerf-Cluster mit Git über SSH geprüft, nicht
  mit dem öffentlichen Repository, einer Registry oder in CI.
- **Binary-Größe.** `panko` wächst durch das Helm-SDK auf rund 98 MB, `panko-admin` auf
  68 MB, `panko-server` auf 49 MB (Image 58 MB).
- **`test/portability_e2e.py`** (Python/Go-Übergänge mit gesicherten Images) ist nicht Teil der
  Suiten und für den eingebetteten Controller nicht mehr aussagekräftig.

## Auswirkungen auf Berechtigungen, Abhängigkeiten und Updates

- **Berechtigungen.** Neu: `panko-controller` (ClusterRole: API-Gruppe
  `platform.example.com` vollständig, `metacontroller.k8s.io`, Jobs, lesend Pods, Namespaces
  und CRDs, Events; Leases nur in `infra-runners`; keine Secrets) ersetzt die breiten
  Standardrechte des Metacontroller-Charts. Neu: `panko-setup` (Secrets erstellen und
  aktualisieren in `infra-*`, erstellen in Team-Namespaces; ConfigMaps und NetworkPolicies in
  Team-Namespaces; Services lesend für Egress-Ziele; keine Ressourcen, keine Workloads).
  Unverändert: Gateway (keine Secrets), Endpoint, Runner je Team mit getrenntem State.
- **Installation.** `panko install` braucht Cluster-Administratorrechte für CRDs, ClusterRoles
  und Namespaces und prüft sie vorab mit `SelfSubjectAccessReview`.
- **Abhängigkeiten.** Neu: Helm SDK v4.3.1 (nur im CLI), Metacontroller v4.17.6 mit
  controller-runtime, zap und Prometheus-Client (nur in `panko-server`), `k8s.io/cli-runtime`.
  OpenTofu und git bleiben externe Werkzeuge (Engine-Image, git-sync, Agent-Image).
- **Updates.** Metacontroller-Updates sind ein Code-Update von Panko (Pseudo-Version in
  `go.mod`, CRDs in `crds/`), kein separates Chart-Update mehr. CRDs aktualisiert Helm nie;
  `panko install` wendet sie vor jedem Upgrade an, GitOps über `CreateReplace` bzw.
  `ServerSideApply`. Das Chart behält Namespaces, Daten-PVCs und StatefulSets bei
  `helm uninstall` und GitOps-Pruning.

## Migration

Bestehende Installationen aus `make deploy` (v0.0.1 und `main` vor diesem PoC) haben
Objekte ohne Helm-Eigentum und einen separaten Metacontroller im Namespace `metacontroller`.

Geprüfter Ablauf (Lab aus `main`, Database `orders` im Zustand Ready):

```sh
panko admin --kubeconfig KC --config panko.json pause          # Hook und Gateway auf 0, laufende Jobs abwarten
helm --kubeconfig KC uninstall metacontroller -n metacontroller
kubectl --kubeconfig KC delete namespace metacontroller
panko install --kubeconfig KC --config panko.json --values VALUES --tokens-file .local/team-tokens.json \
  --take-ownership --yes                                        # Images müssen im Cluster verfügbar sein
panko admin --kubeconfig KC --config panko.json resume
kubectl --kubeconfig KC -n infra-backends delete job s3-onboard --ignore-not-found   # Altlast, nicht im Chart
```

`--take-ownership` übernimmt Namespaces, RBAC, Services, ConfigMaps, StatefulSets, die
CompositeController-Definition und die Metacontroller-CRDs in das Helm-Release und erzwingt dabei
Konflikte mit dem Feldmanager von `kubectl apply`. Die zustandslosen Deployments
(`database-hook`, `platform-gateway`, `runner-endpoint`, `provider-mirror`, `module-mirror`)
werden gelöscht und vom Chart neu angelegt, weil Server-Side Apply fremd verwaltete Felder (etwa
den alten `hook`-Container) nicht entfernen kann. Daten liegen in Secrets, StatefulSets und PVCs
und bleiben unverändert; der Finalizer `metacontroller.io/compositecontroller-database-controller`
hat denselben Namen, der eingebettete Controller setzt die Verwaltung nahtlos fort.

Ergebnis: State, Connection-Secret, Gateway-Tokens, UID, `lastAppliedHash` und Job-Zahl vor und
nach der Migration identisch, kein erneuter Apply. Danach führte eine Spec-Änderung zu einem
erfolgreichen Apply (Generation 2) und das Löschen zu Destroy samt State-Bereinigung.

Der Test deckte drei Fehler auf, die behoben sind: (1) Server-Side-Apply-Konflikte mit
`kubectl-client-side-apply` – Adoption setzt jetzt `ForceConflicts`; (2) `strategy: Recreate`
ist mit dem vorhandenen `rollingUpdate`-Block ungültig – der Controller nutzt `RollingUpdate`
mit `maxSurge: 0`; (3) übernommene Deployments behielten den alten Container – sie werden jetzt
ersetzt (Erkennung über die `last-applied-configuration`-Annotation von `kubectl apply`).

Nicht geprüft: Installationen mit privatem SSH-Modul, Egress-Regeln oder Runner-Pools, sowie
Installationen aus der Zeit vor der State-Isolation (zuerst mit v0.0.1 migrieren, siehe
[Betrieb](operations.de.md#vorhandene-installation-migrieren)). Vor der Migration
`make backup` mit dem alten Stand ausführen.

## Verbleibende Arbeit bis zu einer veröffentlichbaren Version

1. Release-Workflow: `panko-server`, `panko-module-mirror` und Agent-Image mit festen Tags,
   Chart als OCI-Artefakt mit passender `appVersion`, CLI-Archive mit eingebettetem Chart.
2. Die Argo-CD- und Flux-Prüfungen in CI übernehmen (heute manuelle Skripte).
3. Entscheidung zum Hook-Prozess (eigener Container) und zur Umbenennung von `database-hook`.
4. Konfigurierbare Namen vorhandener Secrets oder eine dokumentierte External-Secrets-Vorlage.
5. Automatischer CA-Austausch mit Vertrauensübergang (heute manuell, mit Warnung).
6. Modul-Rollout: Controller erst nach Bereitschaft des Mirrors mit der neuen Revision.
7. Automatische Prüfung der Metacontroller-Version (Renovate-Regel oder CI-Check für
   `replace` und `crds/metacontroller.yaml`).
8. Migration als ein Befehl (Pause, Entfernen des separaten Metacontrollers, Aufräumen der
   Altobjekte, Resume) und als CI-Test mit einem v0.0.1-Lab.
9. Leader-Election und mehrere Controller-Replikate im Lab prüfen (vorhanden, nicht getestet).
10. Dokumentation der Release-Version aktualisieren (Quickstart verweist für 0.0.1 auf den Tag).
