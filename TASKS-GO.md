# Schrittweise Portierung nach Go

Stand: 24. Oktober 2026. Alle Phasen einschließlich Gesamtabnahme abgeschlossen.

Reihenfolge: Grundlagen → Gateway → Controller-Hook → Runner → Verwaltungswerkzeuge → Abschluss.
Jede Komponente wird einzeln umgestellt und vor dem nächsten Schritt geprüft.
Die bestehende [Review-Taskliste](TASKS.md) bleibt als Nachweis der vorherigen Verbesserungen erhalten.

## Rahmen und gemeinsame Abnahme

- CRDs bleiben die Quelle für fachliche Felder, Defaults und Validierung. OpenAPI wird weiterhin daraus abgeleitet; keine separat gepflegten fachlichen Go-Modelle je Ressourcentyp.
- HCL bleibt für die Provisionierung zuständig. YAML-Manifeste und einfache Build-/Cluster-Shellskripte bleiben erhalten.
- Metacontroller bleibt bestehen. Ein eigener Operator mit `controller-runtime` wäre ein separates Vorhaben.
- HTTP-Verträge, Job-Identitäten, Ressourcenstatus, Snapshots, State-Namen, RBAC und Retry-Verhalten bleiben während der Portierung kompatibel.
- Die Python-E2E-Tests bleiben erhalten und prüfen jede neue Implementierung von außen. Direkte Unit-Tests portierter Python-Funktionen werden durch Go-Unit-Tests ersetzt.
- Python-Werkzeuge außerhalb der Laufzeitkomponenten, etwa `tools/build_brand.py`, müssen nicht portiert werden.
- Für jeden Schritt: Go-Tests, Race Detector, Formatierung und `go vet` ausführen; anschließend die bestehenden Cluster-E2E-Tests gegen die gewählte Kombination aus Python- und Go-Komponenten bestehen lassen.
- Eine Komponente gilt erst nach erfolgreichem Rollout und Prüfung als umgestellt. Bis zur Gesamtabnahme müssen die vorherigen Images für einen Rückwechsel verfügbar bleiben.

## 1. Go-Grundlagen und Kompatibilitätstests

Ausgangspunkt: `mise.toml`, `Makefile`, `.github/workflows/ci.yaml`, `test/`.

- [x] Eine unterstützte Go-Version festlegen und in `mise.toml` pinnen; kompatible Versionen der Kubernetes-Bibliotheken auswählen.
- [x] Ein Go-Modul mit `go.mod` und `go.sum` einrichten. Vorgeschlagene Einstiegspunkte: `cmd/gateway`, `cmd/hook`, `cmd/runner`, später `cmd/admin`; gemeinsame Implementierung unter `internal/`.
- [x] Kubernetes-Zugriff auf Basis von `client-go` vorbereiten: In-Cluster-Authentifizierung für Dienste, explizite Kubeconfig für lokale Verwaltungsbefehle, Timeouts und Fehlerabbildung. Abhängigkeiten nur dort einführen, wo sie gebraucht werden.
- [x] Make-Targets und CI für Go-Formatprüfung, `go vet`, Unit-Tests und `go test -race ./...` ergänzen. Die Python-Checks während der Übergangsphase beibehalten.
- [x] Build und lokale Deployment-Auswahl pro Komponente ermöglichen. Vorherige Python-Images vor dem Überschreiben lokaler `:dev`-Tags mit eindeutigen Referenzen sichern.
- [x] Referenzfälle aus der Python-Implementierung festhalten: HTTP-Antworten, OpenAPI, Reconcile-Requests/-Responses, Snapshots, Hashes und Job-Namen. Zeitabhängige Werte gezielt normalisieren; fachliche Arrays und Statuscodes exakt vergleichen.
- [x] Gemeinsame Hash-Testvektoren ergänzen. Die Go-Implementierung muss dieselben Bytes wie die bisherige kanonische Python-JSON-Serialisierung hashen, einschließlich Schlüsselreihenfolge, Escaping, Zahlen und Unicode. Auch die Job-Identität muss identisch bleiben.

**Abnahme:** Die Go-Prüfungen laufen lokal und in der CI-Konfiguration. Die Python-Version besteht weiterhin ihre Tests. Referenzfälle und die komponentenweise Auswahl erlauben einen kontrollierten Vergleich und Rückwechsel.

## 2. Gateway/API portieren

Ausgangspunkt: `gateway/app.py`, `gateway/kube.py`, `gateway/schema.py`, `gateway/Dockerfile`, `gateway/deployment.yaml`, `test/test_gateway.py`.

- [x] HTTP-Server und bestehende Routen mit `net/http` implementieren: Health, Dokumentation, OpenAPI, Metriken sowie generische Collection-/Item-Routen.
- [x] Team-Token-Datei, Bearer-Prüfung und Team-Zuordnung übernehmen. Tokenvergleich gegen Timing-Angriffe absichern und die bisherige Möglichkeit zur Token-Aktualisierung erhalten.
- [x] Kubernetes-CRUD mit dynamischem Client bzw. passendem REST-Zugriff umsetzen. Namespace-Grenze, URL-Encoding, strikte Feldvalidierung, JSON Merge Patch und asynchrones Delete erhalten.
- [x] Fehlervertrag übernehmen: Statuscodes, `message`, `errors[].field`, `errors[].message` und `WWW-Authenticate`. Interne Metadaten und Secrets dürfen nicht in Ressourcenantworten erscheinen.
- [x] CRD-Discovery und OpenAPI-Erzeugung portieren: exponierte namespaced CRDs, served-Versionen, Mehrdeutigkeitsprüfung, Beschreibungen, Nullable- und PATCH-Schemas.
- [x] Discovery-Cache samt konfigurierbarer TTL, serialisiertem Refresh und Fehlerverhalten übernehmen. Strukturierte Auditlogs und Metriknamen/-Labels kompatibel halten.
- [x] Swagger UI weiterhin unter `/docs` anbieten und die bestehende OpenAPI-Clientgenerierung prüfen.
- [x] Gateway-Unit-Tests nach Go übertragen und um HTTP-Vertragstests mit simuliertem Kubernetes-Zugriff ergänzen.
- [x] Go-Image mit unprivilegiertem Benutzer bauen; Read-only-Dateisystem, Token-Mount, ServiceAccount, Ports und Probes erhalten. Geordnetes Herunterfahren und HTTP-Zeitlimits prüfen.
- [x] Nur das Gateway umstellen. Controller und Runner zunächst in Python weiterbetreiben; vollständige E2E-Suite einschließlich RBAC, dynamischer CRD und OpenAPI-Generator ausführen.

**Abnahme:** Go-Gateway und Python-Gateway erfüllen denselben HTTP-/OpenAPI-Vertrag. Team-Isolation, Cache, Logs und Metriken funktionieren. Die Kombination Go-Gateway + Python-Hook + Python-Runner besteht die E2E-Tests.

## 3. Controller-Hook portieren

Ausgangspunkt: `controller/hook.py`, `controller/telemetry.py`, `controller/Dockerfile`, `controller/compositecontroller.yaml`, `test/test_hook.py`, `test/test_telemetry.py`.

- [x] Reconciliation zunächst als reine Go-Funktion mit typisierten Metadaten und generischer Spec implementieren. Fehlende Felder und große Generationsnummern ohne verlustbehaftete Zahlkonvertierung behandeln.
- [x] Metacontroller-Webhooks `/sync` und `/finalize` sowie `/healthz` und `/metrics` übernehmen. Request-/Response-Vertrag und Resync-Verhalten erhalten.
- [x] Job-Erzeugung portieren: Hashes, UID-basierte Namen, Annotationen, Snapshot-Umgebung, Security Context, Ressourcenlimits und Laufzeitgrenzen exakt erhalten.
- [x] Vorhandene Jobs unverändert weiterführen, einschließlich Python-Jobs, die vor dem Rollout gestartet wurden. Keine laufenden Jobs wegen eines Sprachwechsels ersetzen.
- [x] Generationsserialisierung, Ready-/Failed-Status, `lastAppliedHash`, Connection-Secret-Referenz und stabile Condition-Zeitstempel übernehmen.
- [x] Finalisierung und `status.destroy` übernehmen: auf laufende Applies warten, Fehler dauerhaft speichern und neue Versuche ausschließlich über den vereinbarten Retry-Token zulassen.
- [x] Strukturierte Übergangslogs und die bestehenden Prometheus-Metriken übernehmen; Parallelität, Deduplizierung und begrenzten Speicherverbrauch prüfen.
- [x] Controller- und Telemetrie-Unit-Tests nach Go übertragen. Python-/Go-Ergebnisse anhand der Referenzfälle vergleichen; insbesondere TTL, Neustart, Retry, Namenskollisionen und Spec-Änderungen während Apply testen.
- [x] Hook-Image und Deployment umstellen. Metacontroller-Konfiguration und tokenlosen Betrieb des Hooks erhalten.
- [x] Den Wechsel bei einem laufenden Python-Runner-Job und den Rückwechsel des Hooks im Testcluster prüfen. Danach die vollständige E2E-Suite mit Go-Gateway + Go-Hook + Python-Runner ausführen.

**Abnahme:** Der Go-Hook verarbeitet vorhandene Ressourcen und Jobs ohne neue Identitäten oder zusätzliche Applies. Status, Finalizer und explizite Destroy-Retries bleiben kompatibel; die gemischte Installation besteht alle E2E-Tests.

## 4. Runner portieren

Ausgangspunkt: `runner/entrypoint.py`, `runner/Dockerfile`, `runner/tofurc`, `terraform/database/main.tf`, `test/test_runner.py`, `test/runner-smoke.py`.

- [x] CLI-Vertrag `apply|destroy NAMESPACE NAME` sowie `PANKO_RESOURCE`, `PANKO_HASH` und `PANKO_JOB` übernehmen.
- [x] Snapshot-Parsing, Identitätsprüfung und Hash-Prüfung mit denselben Testvektoren implementieren. Spec unverändert an HCL weiterreichen; keine fachlichen Feldzuordnungen einführen.
- [x] Admin-Secret und ServiceAccount-Credentials mit bestehenden Berechtigungen lesen. Zugangsdaten ausschließlich als technische Prozessumgebung an OpenTofu übergeben.
- [x] Modul in das Arbeitsverzeichnis kopieren und `resource.auto.tfvars.json` erzeugen. Team-State-Namespace, Backend-Suffix und Lock-Verhalten unverändert übernehmen.
- [x] OpenTofu-Prozessaufrufe samt Argumenten, Ausgabeströmen und Fehlercodes portieren. SIGTERM/SIGINT an den Kindprozess weiterreichen, auf dessen Ende warten und einen Fehler niemals als erfolgreichen Job melden.
- [x] Destroy-Cleanup übernehmen: State und Lease erst nach erfolgreichem Destroy entfernen; Fehler bei Cleanup melden und bereits fehlende Objekte idempotent behandeln.
- [x] Strukturierte Start-/Ergebnislogs mit identischen Identitätsfeldern implementieren. Tokens und Passwörter aus Argumentlisten und strukturierten Logs ausschließen.
- [x] Go-Runner-Unit-Tests für Snapshot, Prozessfehler, Abbruch und Cleanup ergänzen; den Python-Smoke-Test als externen Test erhalten.
- [x] Runner-Binary in ein Image mit OpenTofu, Provider-Mirror und CA-Zertifikaten übernehmen. Python-Interpreter entfernen; bestehenden Non-root-/Read-only-Betrieb prüfen. Linux AMD64 und ARM64 als Build-Ziele berücksichtigen.
- [x] Prüfen, dass Go-Runner bestehende States aus Python-Läufen aktualisieren und zerstören können. Den Rückwechsel mit denselben States testen; keine Backend- oder State-Migration mit der Sprachportierung verbinden.
- [x] Go-Runner für neue Jobs aktivieren, laufende Python-Jobs fertigstellen lassen und die vollständige E2E-Suite mit allen drei Go-Komponenten ausführen.

**Abnahme:** Go-Runner provisionieren, aktualisieren und löschen Ressourcen mit bestehenden States. Fehler, Prozessabbrüche und Cleanup werden korrekt gemeldet. Die drei Laufzeitkomponenten benötigen keinen Python-Interpreter mehr und bestehen die vorhandenen Python-E2E-Tests.

## 5. RBAC- und Migrationswerkzeuge portieren

Nach der Runtime-Portierung. Ausgangspunkt: `gateway/rbac.py`, `cluster/runner-rbac.py`, `cluster/migrate-state.py`, `cluster/deploy.sh`, `test/test_migration.py`.

- [x] Verwaltungsbefehle unter `cmd/admin` für Gateway-RBAC, Runner-RBAC sowie State-Migration und deren Deployment-Vorprüfung einrichten.
- [x] RBAC-Generierung übernehmen: explizit exponierte Ressourcentypen, Team-ServiceAccounts, getrennte State-Namespaces und eingeschränkter Admin-Secret-Zugriff. Ausgabe semantisch mit der bisherigen Version vergleichen.
- [x] Migrationsschutz vollständig übernehmen: Controller stoppen, auf alte Pods warten, aktive Jobs und belegte Locks erkennen sowie States nach dem Stillstand erneut lesen.
- [x] Private Sicherung mit Dateimodus 0600, konfliktfreie Kopie und Verifikation aller Ziele vor dem Entfernen alter States übernehmen. State- und Token-Inhalte dürfen nicht in Logs erscheinen.
- [x] Migrations-Unit-Tests nach Go übertragen. Abbruch, Wiederaufnahme und Konflikte zusätzlich mit isolierten Test-Secrets im Testcluster prüfen.
- [x] Make-Targets und Deployment-Skripte auf die Go-Werkzeuge umstellen. Ein frisches Lab, ein bestehendes Lab und die dokumentierte Legacy-State-Migration prüfen.

**Abnahme:** Deployment und Verwaltungsabläufe benötigen diese Python-Skripte nicht mehr. Effektive RBAC-Rechte bleiben gleich; Migrationen überschreiben keine abweichenden Ziel-States und behalten ihre Sicherungs- und Abbruchgarantien.

## 6. Abschluss und Bereinigung

- [x] Abschließende Vertragsprüfung über alle Komponenten durchführen: HTTP/OpenAPI, Reconciliation, Hashes, Job-Namen, Status, Snapshots, State-Zugriff, Retry, Logs und Metriken.
- [x] Vollständige Go-Prüfungen und Python-E2E-Suite im frischen Lab ausführen. Den dokumentierten Upgrade-Pfad aus einer Python-Installation separat prüfen.
- [x] Python-Implementierungen und ihre direkten Unit-Tests erst nach erfolgreicher Gesamtabnahme entfernen. Python-E2E-Tests sowie benötigte Testabhängigkeiten und sonstige Python-Werkzeuge erhalten.
- [x] Übergangsschalter und doppelte Build-Pfade entfernen. Vorherige Release-Images für den dokumentierten Rückwechsel weiter verfügbar halten.
- [x] README, Betriebsanleitung, Make-Targets und CI auf den endgültigen Go-/Python-Testaufbau aktualisieren. `go.mod`-/`go.sum`-Pflege und die benötigten lokalen Werkzeuge dokumentieren.
- [x] Prüfnachweise, getestete Image-Referenzen, gemischte Versionen und verbleibende Grenzen in dieser Liste festhalten. Verbesserungen bei Image-Größe, Startzeit und Speicher nur mit Messwerten behaupten.

**Gesamtabnahme:** Gateway, Metacontroller-Hook, Runner und Verwaltungswerkzeuge laufen in Go. CRDs, HCL und die externe Python-E2E-Suite erfüllen weiterhin dieselben Verträge. Upgrade und Rückwechsel sind geprüft; CI baut und testet den endgültigen Aufbau.


## Laufende Prüfnachweise

- Python-Baseline: `panko-gateway:python-baseline-20261023`, `panko-hook:python-baseline-20261023`, `panko-runner:python-baseline-20261023`; ursprüngliche Quellen zusätzlich lokal unter `.local/python-baseline/` gesichert.
- Grundlage: Go 1.27.3, client-go v0.36.5; feste Referenzdaten unter `testdata/` für Hashes, OpenAPI, HTTP und Reconciliation. Hash-Vektoren berücksichtigen Unicode, Escaping, Fließkommazahlen und Ganzzahlen oberhalb der JavaScript-Präzisionsgrenze.
- Phase 2: `panko-gateway:go-stage2` mit Python-Hook und Python-Runner besteht `make test` vollständig. Protokoll: `.local/go-stage2-e2e.log`. Go-Tests mit Race Detector und `go vet` bestehen.
- Phase 3: `panko-hook:go-stage3` besteht Vergleichs- und Race-Tests. Ein aktiver Python-Job blieb beim Wechsel Go → Python → Go unverändert; danach bestand die vollständige E2E-Suite mit Go-Gateway + Go-Hook + Python-Runner. Protokolle: `.local/go-stage3-upgrade.log`, `.local/go-stage3-e2e.log`.
- Phase 4: `panko-runner:go-stage4` besteht den Wechsel Python → Go → Python → Go mit demselben State und unveränderten Zugangsdaten; Go-Destroy samt Cleanup erfolgreich. Der Provider-Mirror enthält die beim Build verifizierten h1-Prüfsummen der Zielplattform. Protokoll: `.local/go-stage4-upgrade.log`.
- Phase 4/5: Vollständige Suite mit allen Go-Laufzeitkomponenten und Go-RBAC bestanden (`.local/go-stage4-e2e.log`). Go-Bootstrap und Go-Deployment im separaten, frisch angelegten Cluster `infra-lab-go` erfolgreich. Reale Migration mit Test-Secrets prüft Locks, Konflikte, Wiederaufnahme, private Backups und Token-Erhalt (`.local/go-stage5-migration.log`).
- Alle vier Go-Kommandos erfolgreich für Linux AMD64 und ARM64 cross-kompiliert. Geordnetes HTTP-Herunterfahren mit laufender Anfrage und Signalweitergabe an Kindprozesse sind mit Race Detector geprüft.
- Frische Go-Installation: vollständige E2E-Suite bestanden (`.local/go-fresh-e2e.log`), inklusive dynamischer CRD, OpenAPI-Clientgenerierung und Observability. Der sprachunabhängige Runner-Smoke-Test besteht separat im frischen Cluster (`.local/go-final-smoke.log`). Go-Images enthalten keinen Python-Interpreter und wurden mit schreibgeschütztem Dateisystem ohne Netzwerk geprüft.
- Abschlussbereinigung: Python-Runtime, direkte Python-Unit-Tests und alte Dockerfiles entfernt; externe Python-E2E-Tests bleiben bestehen. Make-/CI-Unit-Target verwendet Go mit Race Detector. Zusätzlich sind leere, ungültige und nicht als JSON deklarierte HTTP-Bodies durch Python-Referenzfälle abgedeckt.
- Upgrade-Pfad aus einer Python-Installation (separates Lab `infra-lab-upgrade`): Bootstrap und Deployment mit den gesicherten Python-Quellen und den Images `python-baseline-20261023` (Digests im Cluster geprüft), danach `orders` mit dem Python-Stack bereitgestellt. Anschließend In-place-Upgrade per `make build deploy`. Nach zwei Resync-Zyklen mit dem Go-Hook waren UID, Generation, Status samt Condition-Zeitstempel, `lastAppliedHash`, Job, State und Zugangsdaten byteidentisch; kein zusätzlicher Apply (`.local/go-upgrade-before.txt`, `.local/go-upgrade-after.txt`). Go aktualisierte die Python-Ressource mit neuem Apply-Job und unveränderten Zugangsdaten und zerstörte sie vollständig: Connection-Secret, State, Lock, Datenbank und Rolle entfernt (`.local/go-upgrade-lifecycle.log`). Danach bestand die vollständige E2E-Suite im aktualisierten Lab (`.local/go-upgrade-e2e.log`).
- Getestete Go-Images (lokal, Linux ARM64): `panko-gateway:dev` `sha256:4ed86c9ec11fd6334fad5a94d402bfe5a798e829994660396de087b14fef6e06`, `panko-hook:dev` `sha256:04c7ec2338bc0edf363373290b76edf24dc4666f9f1d248ad78fec146d9d3586`, `panko-runner:dev` `sha256:5dc1625a8b2cbeb026d67debd7c97981c71503158fb9c63f65d86b04a2417453`.
- Gemessene Image-Größen Python-Baseline → Go (entpackt laut `docker images` / komprimierter Inhalt laut `docker image inspect`): Gateway 117 MB → 29,9 MB / 27,5 MB → 8,6 MB; Hook 78,9 MB → 23,9 MB / 18,5 MB → 7,1 MB; Runner 280 MB → 230 MB / 81,7 MB → 71,6 MB. Startzeit und Speicherverbrauch wurden nicht gemessen; dazu wird keine Aussage getroffen.
- Verbleibende Grenzen: Laufzeittests nur auf Linux ARM64 (AMD64 nur cross-kompiliert); die GitHub-Actions-Konfiguration wurde lokal über dieselben Make-Targets geprüft, ein Remote-Lauf steht aus. Die Python-Baseline-Images liegen nur lokal und sind nicht in einer Registry veröffentlicht.
