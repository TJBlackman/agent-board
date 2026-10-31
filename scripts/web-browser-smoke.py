from playwright.sync_api import sync_playwright, expect
import json, os
with sync_playwright() as p:
    browser = p.chromium.launch(channel='msedge', headless=True)
    page = browser.new_page(viewport={'width': 1280, 'height': 800})
    page.goto('http://127.0.0.1:18080/ui/login')
    page.get_by_label('Credential', exact=True).fill('ab_browser_fixture_only')
    page.get_by_role('button', name='Sign in', exact=True).click()
    expect(page).to_have_url('http://127.0.0.1:18080/ui/')
    expect(page.locator('article')).to_have_count(50)
    page.get_by_role('link', name='Older posts').click()
    expect(page.locator('article')).to_have_count(2)
    page.get_by_label('Search posts and comments').fill('uniquebrowserneedle')
    page.get_by_role('button', name='Search', exact=True).click()
    page.get_by_role('link', name='Open matching comment').click()
    expect(page.locator('.body')).to_contain_text('uniquebrowserneedle')
    page.get_by_role('link', name='Parent post', exact=True).click()
    expect(page.get_by_role('heading', name='Browser fixture: linked discussion')).to_be_visible()
    expect(page.locator('article.comment')).to_have_count(50)
    assert page.locator('script').count() == 0
    expect(page.locator('.post .body')).to_contain_text('<script>escaped HTML</script>')
    with page.expect_popup() as popup:
        page.get_by_role('link', name='Show JSON', exact=True).first.click()
    raw = popup.value
    raw.wait_for_load_state()
    assert raw.url.endswith('/json'), raw.url
    doc = json.loads(raw.locator('pre').first.text_content())
    assert doc['post']['title'] == 'Browser fixture: linked discussion', doc['post']
    assert len(doc['comments']) == 52 and doc['comment_count'] == 52 and doc['comments_complete'] and doc['next_comments_cursor'] is None
    raw.close()
    page.get_by_role('link', name='Next comments').click()
    expect(page.locator('article.comment')).to_have_count(2)
    page.get_by_role('link', name='In reply to comment').click()
    expect(page.locator('.body')).to_contain_text('uniquebrowserneedle')
    page.get_by_role('link', name='Stats', exact=True).click()
    stats = page.locator('table').inner_text()
    assert 'All time\t52\t52' in stats, stats
    assert 'Past 30 days\t52\t52' in stats, stats
    page.set_viewport_size({'width': 390, 'height': 844})
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
    screenshot = os.path.join(os.environ['TEMP'], 'agent-board-stats-smoke.png')
    page.screenshot(path=screenshot, full_page=True)
    page.get_by_role('button', name='Log out', exact=True).click()
    expect(page.get_by_role('heading', name='Sign in', exact=True)).to_be_visible()
    page.goto('http://127.0.0.1:18080/ui/stats')
    expect(page).to_have_url(__import__('re').compile('/ui/login'))
    print(json.dumps({'passed': ['real-form-login', 'post-pagination', 'comment-search', 'parent-link', 'comment-pagination', 'off-page-reply', 'escaped-body', 'json-tab', 'rolling-stats', 'mobile-layout', 'logout'], 'screenshot': screenshot}))
    browser.close()