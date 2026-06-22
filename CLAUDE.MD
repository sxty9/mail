<holistic_architecture_maxims>
Du arbeitest im "Holistic" Services-Ökosystem. Validiere JEDE Implementierung gegen diese Maximen, bevor du Code schreibst oder änderst:

<maxim name="Single Source of Truth">
- Jede Datenabfrage und jedes Setzen von Daten ist atomar.
- Existiert für die Entität bereits ein Zugangspunkt? Zwingend wiederverwenden. Baue niemals parallele Datenpfade.
</maxim>

<maxim name="Reuse before Build">
1. Suche die Komponente im Holistic SDK.
2. Wenn ähnlich vorhanden: Erweitere die SDK-Komponente.
3. Wenn nicht vorhanden, aber domänenübergreifend: Baue sie im SDK.
4. Nur wenn hochspezifisch: Baue sie lokal in diesem Service.
</maxim>

<maxim name="Uniformity">
- Code-Struktur: Syntaktischer Aufbau, Code-Layout, Naming-Conventions und Repository-Skeletons müssen exakt den anderen Holistic-Services entsprechen.
- CLI: Jeder Service stellt eine CLI bereit. Diese muss sich in Syntax und Semantik strikt an den Holistic-Standard halten. Nutze bestehende Holistic-Services als Referenz.
- Rechtesystem: Einheitliches, symmetrisches Design. Jeder Service stellt ein Rechte-Manifest für den zentralen Privilege Service bereit. Feingranularität ist nur dort erlaubt, wo sie fachlich zwingend geboten ist.
</maxim>

<maxim name="Minimalism">
- Das System ist "intuitiv by Design". Keine Hilfstexte, Notes oder Tooltips im UI (außer bei extremen Spezialfällen).
- Präsentiere Daten nicht im Überfluss, sondern strikt portioniert und bedarfsgerecht.
</maxim>
</holistic_architecture_maxims>

<decision_rule>
Führe keine Änderungen durch, die Redundanz schaffen oder UI-Bloat erzeugen. Bevorzuge Refactoring des SDKs gegenüber lokalem Custom-Code.
</decision_rule>
