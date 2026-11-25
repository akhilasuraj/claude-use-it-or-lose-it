package main

import "encoding/base64"

// logoSVG is the plugin mark shown in the management panel's plugin list: a pastel bubble
// with a mint liquid line, matching the status page.
const logoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">` +
	`<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1">` +
	`<stop offset="0" stop-color="#c9b8ff"/><stop offset=".55" stop-color="#ffb6ca"/><stop offset="1" stop-color="#ffcfae"/>` +
	`</linearGradient><clipPath id="c"><circle cx="32" cy="32" r="28"/></clipPath></defs>` +
	`<circle cx="32" cy="32" r="28" fill="url(#g)"/>` +
	`<path clip-path="url(#c)" d="M0 38 Q16 31 32 38 T64 38 V64 H0Z" fill="#a6ead0"/>` +
	`<circle cx="32" cy="32" r="27" fill="none" stroke="#fff" stroke-opacity=".6" stroke-width="2"/>` +
	`</svg>`

// pluginLogo is base64-encoded so the host's HTML escaping of metadata leaves it intact.
var pluginLogo = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(logoSVG))
