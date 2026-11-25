import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const distDir = path.resolve(__dirname, '../../internal/panel/dist')

if (fs.existsSync(distDir)) {
  for (const entry of fs.readdirSync(distDir)) {
    if (entry === 'placeholder.txt') continue
    fs.rmSync(path.join(distDir, entry), { recursive: true, force: true })
  }
}
