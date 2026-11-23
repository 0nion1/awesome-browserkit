const pad = (value, width = 3) => String(value).padStart(width, '0')

const semanticResult = document.querySelector('#semantic-result')
document.querySelector('#pointer-action').addEventListener('click', () => { semanticResult.textContent = 'Pointer action clicked' })
document.querySelector('#pointer-action').addEventListener('keydown', event => {
  if (event.key === 'Enter' || event.key === ' ') semanticResult.textContent = 'Pointer action activated by keyboard'
})
const dialog = document.querySelector('#review-dialog')
document.querySelector('#open-dialog').addEventListener('click', () => dialog.showModal())
document.querySelector('#close-dialog').addEventListener('click', () => dialog.close())
document.querySelector('#confirm-dialog').addEventListener('click', () => {
  semanticResult.textContent = 'Review confirmed'
  dialog.close()
})

let mutationsPaused = false
let tick = 0
let phase = 0
let generation = 0
const phases = ['amber', 'cyan', 'violet', 'green']
const updateMutationState = () => {
  tick += 1
  document.querySelector('#fast-clock').textContent = `Tick ${pad(tick, 4)}`
  if (tick % 4 === 0) {
    phase = (phase + 1) % phases.length
    document.querySelector('#in-place-action').textContent = `Phase ${phases[phase]}`
  }
  if (tick % 5 === 0) {
    generation += 1
    const replacement = document.createElement('button')
    replacement.id = 'replaced-action'
    replacement.type = 'button'
    replacement.textContent = `Generation ${pad(generation)}`
    replacement.addEventListener('click', () => {
      document.querySelector('#replacement-result').textContent = `Clicked generation ${pad(generation)}`
    })
    document.querySelector('#replaced-action').replaceWith(replacement)
  }
  if (tick % 7 === 0) document.querySelector('#blinking-panel').hidden = !document.querySelector('#blinking-panel').hidden
}
const mutationTimer = setInterval(() => { if (!mutationsPaused) updateMutationState() }, 100)
document.querySelector('#pause-mutations').addEventListener('click', event => {
  mutationsPaused = !mutationsPaused
  event.currentTarget.setAttribute('aria-pressed', String(mutationsPaused))
  event.currentTarget.textContent = mutationsPaused ? 'Resume mutations' : 'Pause mutations'
  document.querySelector('#mutation-state').textContent = mutationsPaused ? 'Mutations paused' : 'Mutations running'
})

let feedPaused = false
let feedGeneration = 0
const renderFeed = () => {
  feedGeneration += 1
  const fragment = document.createDocumentFragment()
  for (let index = 0; index < 12; index += 1) {
    const eventNumber = feedGeneration * 12 + ((index * 5) % 12)
    const item = document.createElement('li')
    const label = document.createElement('span')
    label.textContent = `Event EVT-${pad(eventNumber, 5)} · lane ${(eventNumber % 4) + 1}`
    const action = document.createElement('button')
    action.type = 'button'
    action.textContent = `Inspect event EVT-${pad(eventNumber, 5)}`
    action.addEventListener('click', () => {
      document.querySelector('#feed-result').textContent = `Selected EVT-${pad(eventNumber, 5)} from feed generation ${feedGeneration}`
    })
    item.append(label, action)
    fragment.append(item)
  }
  document.querySelector('#live-feed').replaceChildren(fragment)
}
renderFeed()
const feedTimer = setInterval(() => { if (!feedPaused) renderFeed() }, 100)
document.querySelector('#pause-feed').addEventListener('click', event => {
  feedPaused = !feedPaused
  event.currentTarget.setAttribute('aria-pressed', String(feedPaused))
  event.currentTarget.textContent = feedPaused ? 'Resume live feed' : 'Pause live feed'
  document.querySelector('#feed-state').textContent = feedPaused ? `Feed paused at generation ${feedGeneration}` : 'Feed running'
})

let asyncRun = 0
const startAsyncReport = async fail => {
  const run = ++asyncRun
  const panel = document.querySelector('#async-panel')
  const state = document.querySelector('#async-state')
  const summary = document.querySelector('#summary-slot')
  const details = document.querySelector('#detail-list')
  document.querySelector('#start-async').disabled = true
  document.querySelector('#start-error').disabled = true
  panel.setAttribute('aria-busy', 'true')
  summary.replaceChildren()
  details.replaceChildren()
  state.textContent = fail ? 'Loading report expected to fail' : 'Loading summary'
  try {
    if (fail) {
      const response = await fetch('/api/snapshot/failure?delay_ms=240')
      if (!response.ok) throw new Error(`HTTP ${response.status}`)
      return
    }
    const summaryResponse = await fetch('/api/snapshot/summary?delay_ms=240')
    if (!summaryResponse.ok) throw new Error(`Summary HTTP ${summaryResponse.status}`)
    const summaryData = await summaryResponse.json()
    if (run !== asyncRun) return
    summary.textContent = `Summary ready: ${summaryData.total} records owned by ${summaryData.owner}`
    state.textContent = 'Summary ready; loading details'
    const detailsResponse = await fetch('/api/snapshot/details?delay_ms=720')
    if (!detailsResponse.ok) throw new Error(`Details HTTP ${detailsResponse.status}`)
    const detailData = await detailsResponse.json()
    if (run !== asyncRun) return
    for (const entry of detailData) {
      const item = document.createElement('li')
      item.textContent = `${entry.id}: ${entry.state}`
      details.append(item)
    }
    state.textContent = 'Async complete'
  } catch (error) {
    if (run === asyncRun) state.textContent = `Async error: ${error.message}`
  } finally {
    if (run === asyncRun) {
      panel.setAttribute('aria-busy', 'false')
      document.querySelector('#start-async').disabled = false
      document.querySelector('#start-error').disabled = false
    }
  }
}
document.querySelector('#start-async').addEventListener('click', () => startAsyncReport(false))
document.querySelector('#start-error').addEventListener('click', () => startAsyncReport(true))

const shadow = document.querySelector('#snapshot-shadow-host').attachShadow({mode: 'open'})
shadow.innerHTML = '<label for="shadow-query">Shadow query</label><input id="shadow-query"><button id="shadow-submit" type="button">Run shadow search</button><p id="shadow-result">Shadow idle</p>'
shadow.querySelector('#shadow-submit').addEventListener('click', () => {
  shadow.querySelector('#shadow-result').textContent = `Shadow result: ${shadow.querySelector('#shadow-query').value || 'empty'}`
})

const dense = document.querySelector('#dense-actions')
for (let index = 1; index <= 160; index += 1) {
  const action = document.createElement('button')
  action.type = 'button'
  action.textContent = `Dense action ${pad(index)}`
  action.addEventListener('click', () => { document.querySelector('#dense-result').textContent = `Selected dense action ${pad(index)}` })
  dense.append(action)
}

const largeSelect = document.querySelector('#large-select')
for (let index = 1; index <= 105; index += 1) {
  const option = document.createElement('option')
  option.value = `option-${pad(index)}`
  option.textContent = `Option ${pad(index)}`
  largeSelect.append(option)
}
const closedShadow = document.querySelector('#closed-shadow-host').attachShadow({mode: 'closed'})
closedShadow.innerHTML = '<button id="closed-shadow-action" type="button">Closed shadow action</button>'
for (const id of ['inert-action', 'covered-action', 'app-owned-ref']) {
  document.querySelector(`#${id}`).addEventListener('click', () => {
    document.querySelector('#fidelity-result').textContent = `Activated ${id}`
  })
}

window.addEventListener('pagehide', () => {
  clearInterval(mutationTimer)
  clearInterval(feedTimer)
})
