import hljs from 'highlight.js/lib/core'
import ini from 'highlight.js/lib/languages/ini'
import yaml from 'highlight.js/lib/languages/yaml'

// Register only TOML (ini) and YAML to keep the embedded bundle lean.
hljs.registerLanguage('toml', ini)
hljs.registerLanguage('ini', ini)
hljs.registerLanguage('yaml', yaml)

export default hljs
