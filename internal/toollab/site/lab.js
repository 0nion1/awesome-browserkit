document.querySelector('#customer-form').addEventListener('submit', event => {
  event.preventDefault()
  document.querySelector('#form-result').textContent = `Saved: ${document.querySelector('#customer').value}`
})

const updateControls = () => {
  document.querySelector('#control-result').textContent = `Reference: ${document.querySelector('#reference-code').value || 'empty'}; Express: ${document.querySelector('#express').checked ? 'yes' : 'no'}; Priority: ${document.querySelector('#priority').value}`
}
for (const id of ['reference-code', 'express', 'priority']) {
  document.getElementById(id).addEventListener('input', updateControls)
  document.getElementById(id).addEventListener('change', updateControls)
}

document.querySelector('#hover-action').addEventListener('click', () => {
  document.querySelector('#hover-result').textContent = 'Hover action triggered'
})

document.querySelector('#load-items').addEventListener('click', async () => {
  const loading = document.querySelector('#loading')
  loading.hidden = false
  try {
    const response = await fetch('/api/items')
    if (!response.ok) throw new Error(`Items request failed: ${response.status}`)
    const items = await response.json()
    document.querySelector('#items').replaceChildren(...items.map(name => {
      const item = document.createElement('li')
      item.textContent = name
      return item
    }))
    document.querySelector('#load-result').textContent = `Loaded ${items.length} items`
    console.info('lab:items-loaded')
  } finally {
    loading.hidden = true
  }
})

document.querySelector('#replace-target').addEventListener('click', () => {
  const replacement = document.createElement('button')
  replacement.id = 'ephemeral'
  replacement.type = 'button'
  replacement.textContent = 'New target'
  document.querySelector('#ephemeral').replaceWith(replacement)
  document.querySelector('#replace-result').textContent = 'Target replaced'
})

document.querySelector('#nested-target').addEventListener('click', () => {
  document.querySelector('#nested-result').textContent = 'Nested target triggered'
})

document.querySelector('#far-target').addEventListener('click', () => {
  document.querySelector('#far-result').textContent = 'Far target triggered'
})

const shadow = document.querySelector('#shadow-host').attachShadow({ mode: 'open' })
shadow.innerHTML = '<label for="shadow-code">Shadow code</label><input id="shadow-code"><button id="shadow-action">Shadow action</button><p id="shadow-result">Shadow idle</p>'
shadow.querySelector('#shadow-action').addEventListener('click', () => {
  shadow.querySelector('#shadow-result').textContent = `Shadow clicked: ${shadow.querySelector('#shadow-code').value}`
})

document.querySelector('#attachment').addEventListener('change', async event => {
  const file = event.target.files?.[0]
  document.querySelector('#upload-result').textContent = file
    ? `Uploaded: ${file.name}; bytes: ${file.size}; content: ${await file.text()}`
    : 'No attachment'
})
