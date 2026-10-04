Vendored for the dashboard on the character grid (#110).

WebTUI CSS 0.1.10 (static/vendor/webtui/full.css, MIT, LICENSE beside it) from the npm package @webtui/css; see static/vendor/webtui/VERSION. It styles the page chrome (title, login state); the grid itself is drawn by package grid and styled once in static/css/grid.css.

JetBrains Mono v2.304 (static/fonts/jetbrains-mono.woff2 Regular, jetbrains-mono-bold.woff2 Bold), SIL OFL (OFL-JetBrainsMono.txt), from https://github.com/JetBrains/JetBrainsMono/releases/tag/v2.304. Full character set (1363 codepoints): the character grid needs box drawing, blocks and geometric shapes from the same font, or columns misalign. static/fonts/jetbrains-mono.css is the @font-face glue.

Datastar + Rocket build: static/vendor/datastar-rocket.js (Datastar v1.0.4 + Rocket beta.2, patched: patches/rocket). It keeps the signals ($_s snapshot, $_h history) live and applies the row patches the stream sends.
