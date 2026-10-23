# Verbesserungen aus dem Code-Review

Historischer Review-Stand vor der Go-Portierung: Die genannten Python-Pfade und
Unit-Tests wurden inzwischen durch Go ersetzt. Aktueller Aufbau und Prüfnachweise:
[TASKS-GO.md](TASKS-GO.md) und [Go-Portierung](docs/go-porting.de.md).

Stand: 23. Oktober 2026. Alle sechs Aufgaben sind umgesetzt und lokal geprüft.
Der GitHub-Actions-Workflow ist eingerichtet und mit actionlint validiert; ein
Remote-CI-Lauf wurde ohne Push nicht ausgelöst.

Prioritäten: **P1** = zuerst bearbeiten, **P2** = danach, **P3** = bei wachsender Nutzung.
Empfohlene Reihenfolge: 1 → 2 → 3 → 4 → 5 → 6. Tests für einzelne Änderungen gehören jeweils zur Umsetzung.

## 1. Eindeutige Job-Identität sicherstellen · P1

Ausgangspunkt: `controller/hook.py`, `test/test_hook.py`.
Zwei Ressourcen mit gleichen ersten 40 Namenszeichen, gleicher Spec und gleicher Generation erzeugen aktuell denselben Job-Namen. Lokal reproduziert.

- [x] Job-Identität aus Ressourcen-UID, Konfigurationshash und Aktion ableiten; die zulässige Namenslänge einhalten.
- [x] Erzeugung und Wiederverwendung bestehender Jobs konsistent anpassen; den Übergang für bereits laufende Jobs berücksichtigen.
- [x] Regressionstests für gleiche Namenspräfixe und für neu angelegte Ressourcen mit gleichem Namen, aber anderer UID ergänzen.
- [x] Deterministische Namen und getrennte Apply-/Destroy-Identitäten testen.

**Abnahme:** Verschiedene Ressourcen erhalten verschiedene Job-Namen. Wiederholte Reconciliation derselben Ausführung behält deren Identität und erzeugt keinen zusätzlichen Job.

## 2. Destroy-Fehler und Wiederholungen explizit behandeln · P1

Ausgangspunkt: `controller/hook.py`, `api/database.yaml`, `test/test_hook.py`, `README.md`.
Nach Ablauf der Job-TTL plant der Controller einen fehlgeschlagenen Destroy erneut. Das widerspricht der dokumentierten manuellen Wiederholung. Lokal reproduziert.

- [x] Retry-Verhalten festlegen: explizite Anforderung oder begrenzte automatische Wiederholungen mit wachsendem Abstand.
- [x] Destroy-Versuch und Ergebnis dauerhaft im Ressourcenstatus speichern, unabhängig von der Existenz des Jobs.
- [x] Retry-Auslösung und Job-Identität aufeinander abstimmen, damit ein neuer Versuch eindeutig ist.
- [x] Tests für Fehler vor und nach Job-TTL, Controller-Neustart und erneuten Versuch ergänzen.
- [x] README und technische Dokumentation dem gewählten Verhalten anpassen.

**Abnahme:** Das Entfernen eines fehlgeschlagenen Jobs durch die TTL löst keine unbeabsichtigte Wiederholung aus. Bei Fehlern bleibt der Finalizer erhalten; ein erfolgreicher Destroy gibt ihn frei.

## 3. Runner-Berechtigungen und Secrets isolieren · P1 vor unabhängigem Mehrteam-Betrieb

Ausgangspunkt: `cluster/runner-rbac.py`, `cluster/deploy.sh`, `gateway/deployment.yaml`, `runner/entrypoint.py`.
Runner dürfen alle Secrets in `infra-runners` lesen und verändern, einschließlich `gateway-auth` und der States anderer Teams.

- [x] Gateway-Authentifizierungssecrets aus dem für Runner zugänglichen State-Namespace verlagern und Deployment sowie Bootstrap anpassen.
- [x] State und Lock-Leases pro Team isolieren; Backend-Konfiguration und Cleanup entsprechend ändern.
- [x] Migrationsweg für vorhandene States und Secrets dokumentieren; laufende Ausführungen berücksichtigen.
- [x] RBAC-Prüfungen ergänzen: Runner eines Teams dürfen weder Gateway-Tokens noch State oder Locks eines anderen Teams lesen oder verändern.
- [x] Apply-/Destroy-E2E-Tests für beide Teams mit der neuen Aufteilung ausführen.
- [x] Verbleibende Vertrauensgrenzen dokumentieren, insbesondere den gemeinsamen PostgreSQL-Adminzugang und die Berechtigungen auf Team-Secrets.

**Abnahme:** Teamübergreifende State-Zugriffe und Zugriffe auf Gateway-Tokens werden durch Kubernetes-RBAC verweigert. Die regulären Lebenszyklen beider Teams funktionieren weiterhin.

## 4. Ausführung an eine nachvollziehbare Konfiguration binden · P2

Ausgangspunkt: `controller/hook.py`, `runner/entrypoint.py`, `terraform/database/main.tf`.
Jobs tragen eine Generation, lesen die aktuelle Ressourcenkonfiguration aber erst während der Ausführung. Zwischenzeitliche Änderungen können damit die Zuordnung von Status und angewendeter Spec verfälschen.

- [x] Einen Testfall für eine Spec-Änderung zwischen Job-Erstellung und Konfigurationsabruf ergänzen.
- [x] Verfahren wählen: unveränderlicher Konfigurationsstand pro Ausführung oder belastbare Erfassung der tatsächlich verarbeiteten Generation und Spec.
- [x] Status und `lastAppliedHash` ausschließlich auf die nachweislich verarbeitete Konfiguration beziehen.
- [x] Änderungen während eines laufenden Apply sowie anschließendes Löschen testen.
- [x] Den Ausführungsvertrag zwischen Controller, Runner und Modul dokumentieren.

**Abnahme:** Ein Job meldet ausschließlich die von ihm verarbeitete Konfiguration als erfolgreich. Neuere Änderungen werden anschließend zuverlässig abgearbeitet.

## 5. Clusterfreie Tests und CI ergänzen · P2

Ausgangspunkt: `test/`, `gateway/`, `Makefile`, `test/e2e.sh`.
Die drei vorhandenen Controller-Unit-Tests bestanden beim Review. Eine CI-Konfiguration wurde nicht gefunden; die vorhandenen E2E-Tests wurden im Review nicht ausgeführt.

- [x] Einen separaten Make-Target für schnelle Tests ohne Cluster und Docker ergänzen.
- [x] Gateway-Tests für fehlende, ungültige und teamfremde Tokens mit simuliertem Kubernetes-Zugriff ergänzen.
- [x] Fehlerabbildung bei Kubernetes-Validierungsfehlern und Nichterreichbarkeit testen.
- [x] CRD-Discovery und OpenAPI-Erzeugung testen, einschließlich mehrdeutiger Routen, dynamischer Ressourcen und PATCH-Schemas.
- [x] Repository-passende CI für Unit-Tests, Python-Linting sowie Terraform-Formatierung und -Validierung einrichten.
- [x] Einen separaten CI-Job mit lokalem Testcluster für die bestehenden Lebenszyklus- und Gateway-E2E-Tests einrichten.
- [x] Testbefehle und benötigte Voraussetzungen dokumentieren.

**Abnahme:** Schnelle Tests laufen ohne Cluster. CI meldet fehlgeschlagene Checks zuverlässig und prüft die bestehenden End-to-End-Abläufe in einem separaten Job.

## 6. Discovery-Aufwand und Betriebsdiagnose verbessern · P3

Ausgangspunkt: `gateway/app.py`, `gateway/kube.py`, `controller/hook.py`, `runner/entrypoint.py`.
Das Gateway lädt exponierte CRDs bei jedem autorisierten Ressourcenaufruf erneut. Logs und Metriken unterstützen die Zuordnung von Fehlern zu einzelnen Ausführungen bisher nur begrenzt.

- [x] Einen kurzen, konfigurierbaren Discovery-Cache einführen und Verhalten bei Ablauf sowie Discovery-Fehlern festlegen.
- [x] Cache-Zugriffe bei parallelen Requests berücksichtigen.
- [x] Tests für Cache-Treffer, Ablauf und Sichtbarkeit neu exponierter CRDs ergänzen.
- [x] Strukturierte Logs mit Ressourcen-UID, Namespace, Name, Generation, Aktion und Job-Identität ergänzen; Tokens und Zugangsdaten ausschließen.
- [x] Metriken für fehlgeschlagene Ausführungen, Provisionierungsdauer und lange blockierte Löschungen ergänzen; Metrik-Labels mit begrenzter Kardinalität verwenden.
- [x] Kurze Diagnoseanleitung für fehlgeschlagene Applies und blockierte Deletes dokumentieren.

**Abnahme:** Wiederholte API-Anfragen innerhalb der Cache-Laufzeit verursachen keinen erneuten CRD-Abruf. CRD-Änderungen werden innerhalb der dokumentierten Frist sichtbar. Fehler lassen sich einer Ausführung zuordnen, ohne Zugangsdaten offenzulegen.


## Prüfnachweise

| Aufgabe | Umsetzung und Nachweis |
| --- | --- |
| 1 · Job-Identität | UID, Konfigurationshash, Aktion und Versuch bestimmen die Identität. Unit-Tests prüfen deterministische Namen, Neuanlage mit anderer UID und unveränderte Legacy-Templates. Der Cluster-E2E-Test provisioniert zwei Ressourcen mit identischen ersten 40 Namenszeichen unabhängig voneinander. |
| 2 · Destroy-Retry | `status.destroy` speichert Versuch und Ergebnis. Eine neue `platform.example.com/retry-destroy`-Annotation erlaubt den nächsten Versuch. Tests prüfen Fehler vor und nach TTL, einen frischen Python-Prozess, eindeutige Retries und erfolgreiche Finalisierung. |
| 3 · Isolation | Gateway in `infra-gateway`, States und Locks in `panko-state-team-a` bzw. `panko-state-team-b`. Effektive RBAC-Prüfungen mit ServiceAccount-Impersonation bestehen; beide Teams durchlaufen Apply und Destroy. Migration, Konfliktvermeidung und verbleibende Vertrauensgrenzen sind dokumentiert. |
| 4 · Snapshot | Neue Jobs erhalten einen festen Ressourcen-Snapshot; Runner prüfen Identität und Hash und reichen ihn direkt an HCL weiter. Tests decken nachträgliche Spec-Änderungen, Folgegenerationen und Delete während eines Apply ab. E2E prüft die ausgeführte Generation im tatsächlichen Runner-Log. |
| 5 · Tests und CI | `make test-unit lint validate` und GitHub Actions mit separatem k3d-E2E-Job sind eingerichtet. 30 Unit-Tests, Ruff, OpenTofu-Formatierung/-Validierung, actionlint und Shell-Syntaxprüfungen bestehen. |
| 6 · Diagnose | Serialisierter, konfigurierbarer Discovery-Cache mit Fehler- und Parallelitätstests; strukturierte Logs und Prometheus-Endpunkte. Live-E2E prüft Cache-Metriken, Hook-Metriken, Logs und dynamische CRD-Sichtbarkeit. Betriebsanleitung enthält Diagnose und Metrikgrenzen. |

Der vollständige lokale Lauf `make test` bestand inklusive PostgreSQL-Lebenszyklen,
RBAC, Namenskollisionen, Gateway-CRUD, Schema-/CEL-Validierung, dynamischer CRD,
OpenAPI-Validierung, Python-Clientgenerierung und Observability-Prüfungen.
Danach waren beide Team-Namespaces frei von Test-CRs und Jobs sowie beide
State-Namespaces frei von Secrets und Leases. Gateway und Hook waren jeweils 1/1 bereit.

Lokale, ignorierte Prüfprotokolle: `.local/unit-test.log`,
`.local/tasklist-final-test.log` und `.local/tasklist-final-deploy.log`.
Betriebs-, Migrations- und Testanleitung: [docs/operations.de.md](docs/operations.de.md).
